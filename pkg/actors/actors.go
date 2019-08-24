package actors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mitchellh/mapstructure"

	"github.com/actionscore/actions/pkg/channel/http"

	"github.com/golang/protobuf/ptypes/any"

	log "github.com/Sirupsen/logrus"
	"github.com/actionscore/actions/pkg/channel"
	"github.com/actionscore/actions/pkg/components/state"
	"github.com/actionscore/actions/pkg/placement"
	pb "github.com/actionscore/actions/pkg/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Actors allow calling into virtual actors as well as actor state mangements
type Actors interface {
	Call(req *CallRequest) (*CallResponse, error)
	Init() error
	GetState(req *GetStateRequest) (*StateResponse, error)
	SaveState(req *SaveStateRequest) error
	DeleteState(req *DeleteStateRequest) error
	TransactionalStateOperation(req *TransactionalRequest) error
	CreateReminder(req *CreateReminderRequest) error
	DeleteReminder(req *DeleteReminderRequest) error
	CreateTimer(req *CreateTimerRequest) error
	DeleteTimer(req *DeleteTimerRequest) error
}

type actorsRuntime struct {
	appChannel          channel.AppChannel
	store               state.StateStore
	placementTableLock  *sync.RWMutex
	placementTables     *placement.PlacementTables
	placementSignal     chan struct{}
	placementBlock      bool
	operationUpdateLock *sync.Mutex
	grpcConnectionFn    func(address string) (*grpc.ClientConn, error)
	config              Config
	actorsTable         *sync.Map
	activeTimers        *sync.Map
	activeReminders     *sync.Map
	remindersLock       *sync.RWMutex
	reminders           map[string][]Reminder
	evaluationLock      *sync.RWMutex
	evaluationBusy      bool
	evaluationChan      chan bool
}

const (
	idHeader        = "id"
	lockOperation   = "lock"
	unlockOperation = "unlock"
	updateOperation = "update"
)

// NewActors create a new actors runtime with given config
func NewActors(stateStore state.StateStore, appChannel channel.AppChannel, grpcConnectionFn func(address string) (*grpc.ClientConn, error), config Config) Actors {
	return &actorsRuntime{
		appChannel:          appChannel,
		config:              config,
		store:               stateStore,
		placementTableLock:  &sync.RWMutex{},
		placementTables:     &placement.PlacementTables{Entries: make(map[string]*placement.Consistent)},
		operationUpdateLock: &sync.Mutex{},
		grpcConnectionFn:    grpcConnectionFn,
		actorsTable:         &sync.Map{},
		activeTimers:        &sync.Map{},
		activeReminders:     &sync.Map{},
		remindersLock:       &sync.RWMutex{},
		reminders:           map[string][]Reminder{},
		evaluationLock:      &sync.RWMutex{},
		evaluationBusy:      false,
		evaluationChan:      make(chan bool),
	}
}

func (a *actorsRuntime) Init() error {
	if a.config.PlacementServiceAddress == "" {
		return errors.New("couldn't connect to placement service: address is empty")
	}

	go a.connectToPlacementService(a.config.PlacementServiceAddress, a.config.HostAddress, a.config.HeartbeatInterval)
	a.startDeactivationTicker(a.config.ActorDeactivationScanInterval, a.config.ActorIdleTimeout)

	log.Infof("actor runtime started. actor idle timeout: %s. actor scan interval: %s",
		a.config.ActorIdleTimeout.String(), a.config.ActorDeactivationScanInterval.String())

	return nil
}

func (a *actorsRuntime) constructCombinedActorKey(actorType, actorID string) string {
	return fmt.Sprintf("%s-%s", actorType, actorID)
}

func (a *actorsRuntime) deactivateActor(actorType, actorID string) error {
	req := channel.InvokeRequest{
		Method:   fmt.Sprintf("actors/%s/%s", actorType, actorID),
		Metadata: map[string]string{http.HTTPVerb: http.Delete},
	}

	resp, err := a.appChannel.InvokeMethod(&req)
	if err != nil {
		return err
	}

	if a.getStatusCodeFromMetadata(resp.Metadata) != 200 {
		return errors.New("error from actor sdk")
	}

	return nil
}

func (a *actorsRuntime) getStatusCodeFromMetadata(metadata map[string]string) int {
	code := metadata[http.HTTPStatusCode]
	if code != "" {
		statusCode, err := strconv.Atoi(code)
		if err == nil {
			return statusCode
		}
	}

	return 200
}

func (a *actorsRuntime) getActorTypeAndIDFromKey(key string) (string, string) {
	arr := strings.Split(key, "-")
	return arr[0], arr[1]
}

func (a *actorsRuntime) startDeactivationTicker(interval, actorIdleTimeout time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for t := range ticker.C {
			a.actorsTable.Range(func(key, value interface{}) bool {
				actorInstance := value.(*actor)

				if actorInstance.busy {
					return true
				}

				durationPassed := t.Sub(actorInstance.lastUsedTime)
				if durationPassed >= actorIdleTimeout {
					go func(actorKey string) {
						actorType, actorID := a.getActorTypeAndIDFromKey(actorKey)
						err := a.deactivateActor(actorType, actorID)
						if err != nil {
							log.Warnf("failed to deactivate actor %s: %s", actorKey, err)
						} else {
							a.actorsTable.Delete(actorKey)
						}
					}(key.(string))
				}

				return true
			})
		}
	}()
}

func (a *actorsRuntime) Call(req *CallRequest) (*CallResponse, error) {
	targetActorAddress := a.lookupActorAddress(req.ActorType, req.ActorID)
	if targetActorAddress == "" {
		return nil, fmt.Errorf("error finding address for actor type %s with id %s", req.ActorType, req.ActorID)
	}

	if a.placementBlock {
		<-a.placementSignal
	}

	var resp []byte
	var err error

	if a.isActorLocal(targetActorAddress, a.config.HostAddress, a.config.Port) {
		resp, err = a.callLocalActor(req.ActorType, req.ActorID, req.Method, req.Data, req.Metadata)
	} else {
		resp, err = a.callRemoteActor(targetActorAddress, req.ActorType, req.ActorID, req.Method, req.Data)
	}

	if err != nil {
		return nil, err
	}

	return &CallResponse{
		Data: resp,
	}, nil
}

func (a *actorsRuntime) callLocalActor(actorType, actorID, actorMethod string, data []byte, metadata map[string]string) ([]byte, error) {
	key := a.constructCombinedActorKey(actorType, actorID)

	val, exists := a.actorsTable.LoadOrStore(key, &actor{
		lock:         &sync.RWMutex{},
		busy:         true,
		lastUsedTime: time.Now(),
	})

	act := val.(*actor)
	lock := act.lock
	lock.Lock()
	defer lock.Unlock()

	if !exists {
		err := a.tryActivateActor(actorType, actorID)
		if err != nil {
			a.actorsTable.Delete(key)
			return nil, err
		}
	} else {
		act.busy = true
		act.lastUsedTime = time.Now()
	}

	method := fmt.Sprintf("actors/%s/%s/method/%s", actorType, actorID, actorMethod)
	req := channel.InvokeRequest{
		Method:   method,
		Payload:  data,
		Metadata: map[string]string{http.HTTPVerb: http.Put},
	}
	if metadata != nil {
		for k, v := range metadata {
			req.Metadata[k] = v
		}
	}

	resp, err := a.appChannel.InvokeMethod(&req)
	act.busy = false

	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

func (a *actorsRuntime) callRemoteActor(targetAddress, actorType, actorID, actorMethod string, data []byte) ([]byte, error) {
	req := pb.CallActorEnvelope{
		ActorType: actorType,
		ActorID:   actorID,
		Method:    actorMethod,
		Data:      &any.Any{Value: data},
	}

	conn, err := a.grpcConnectionFn(targetAddress)
	if err != nil {
		return nil, err
	}

	client := pb.NewActionsClient(conn)
	resp, err := client.CallActor(context.Background(), &req)
	if err != nil {
		return nil, err
	}

	return resp.Data.Value, nil
}

func (a *actorsRuntime) tryActivateActor(actorType, actorID string) error {
	// Send the activation signal to the app
	req := channel.InvokeRequest{
		Method:   fmt.Sprintf("actors/%s/%s", actorType, actorID),
		Metadata: map[string]string{http.HTTPVerb: http.Post},
		Payload:  nil,
	}

	resp, err := a.appChannel.InvokeMethod(&req)
	if err != nil || a.getStatusCodeFromMetadata(resp.Metadata) != 200 {
		key := a.constructCombinedActorKey(actorType, actorID)
		a.actorsTable.Delete(key)
		return fmt.Errorf("error activating actor type %s with id %s: %s", actorType, actorID, err)
	}

	return nil
}

func (a *actorsRuntime) isActorLocal(targetActorAddress, hostAddress string, grpcPort int) bool {
	return strings.Contains(targetActorAddress, "localhost") || strings.Contains(targetActorAddress, "127.0.0.1") ||
		targetActorAddress == fmt.Sprintf("%s:%v", hostAddress, grpcPort)
}

func (a *actorsRuntime) GetState(req *GetStateRequest) (*StateResponse, error) {
	key := a.constructActorStateKey(req.ActorType, req.ActorID, req.Key)
	resp, err := a.store.Get(&state.GetRequest{
		Key: key,
	})
	if err != nil {
		return nil, err
	}

	return &StateResponse{
		Data: resp.Data,
	}, nil
}

func (a *actorsRuntime) TransactionalStateOperation(req *TransactionalRequest) error {
	actorKey := a.constructCombinedActorKey(req.ActorType, req.ActorID)
	val, exists := a.actorsTable.Load(actorKey)
	if !exists {
		return fmt.Errorf("actor id %s is not activated", req.ActorID)
	}

	act := val.(*actor)
	lock := act.lock
	lock.Lock()
	defer lock.Unlock()

	requests := []state.TransactionalRequest{}
	for _, o := range req.Operations {
		switch o.Operation {
		case Upsert:
			var upsert TransactionalUpsert
			err := mapstructure.Decode(o.Request, &upsert)
			if err != nil {
				return err
			}
			key := a.constructActorStateKey(req.ActorType, req.ActorID, upsert.Key)
			requests = append(requests, state.TransactionalRequest{
				Request: state.SetRequest{
					Key:   key,
					Value: upsert.Data,
				},
				Operation: state.Upsert,
			})
		case Delete:
			var delete TransactionalDelete
			err := mapstructure.Decode(o.Request, &delete)
			if err != nil {
				return err
			}

			key := a.constructActorStateKey(req.ActorType, req.ActorID, delete.Key)
			requests = append(requests, state.TransactionalRequest{
				Request: state.DeleteRequest{
					Key: key,
				},
				Operation: state.Delete,
			})
		default:
			return fmt.Errorf("operation type %s not supported", o.Operation)
		}
	}

	transactionalStore, ok := a.store.(state.TransactionalStateStore)
	if !ok {
		return errors.New("state store does not support transactions")
	}
	err := transactionalStore.Multi(requests)
	return err
}

func (a *actorsRuntime) SaveState(req *SaveStateRequest) error {
	actorKey := a.constructCombinedActorKey(req.ActorType, req.ActorID)
	val, exists := a.actorsTable.Load(actorKey)
	if !exists {
		return fmt.Errorf("actor id %s is not activated", req.ActorID)
	}

	act := val.(*actor)
	lock := act.lock
	lock.Lock()
	defer lock.Unlock()

	key := a.constructActorStateKey(req.ActorType, req.ActorID, req.Key)
	err := a.store.Set(&state.SetRequest{
		Value: req.Data,
		Key:   key,
	})
	return err
}

func (a *actorsRuntime) DeleteState(req *DeleteStateRequest) error {
	actorKey := a.constructCombinedActorKey(req.ActorType, req.ActorID)
	val, exists := a.actorsTable.Load(actorKey)
	if !exists {
		return fmt.Errorf("actor id %s is not activated", req.ActorID)
	}

	act := val.(*actor)
	lock := act.lock
	lock.Lock()
	defer lock.Unlock()

	key := a.constructActorStateKey(req.ActorType, req.ActorID, req.Key)
	err := a.store.Delete(&state.DeleteRequest{
		Key: key,
	})
	return err
}

func (a *actorsRuntime) constructActorStateKey(actorType, actorID, key string) string {
	return fmt.Sprintf("%s-%s-%s-%s", a.config.ActionsID, actorType, actorID, key)
}

func (a *actorsRuntime) connectToPlacementService(placementAddress, hostAddress string, heartbeatInterval time.Duration) {
	log.Infof("actors: starting connection attempt to placement service at %s", placementAddress)
	stream := a.getPlacementClientPersistently(placementAddress, hostAddress)

	log.Infof("actors: established connection to placement service at %s", placementAddress)

	go func() {
		for {
			host := pb.Host{
				Name:     hostAddress,
				Load:     1,
				Entities: a.config.HostedActorTypes,
				Port:     int64(a.config.Port),
			}

			if stream != nil {
				if err := stream.Send(&host); err != nil {
					log.Error("actors: connection failure to placement service: retrying")
					stream = a.getPlacementClientPersistently(placementAddress, hostAddress)
				}
			}
			time.Sleep(heartbeatInterval)
		}
	}()

	go func() {
		for {
			resp, err := stream.Recv()
			if err != nil {
				log.Error("actors: connection failure to placement service: retrying")
				stream = a.getPlacementClientPersistently(placementAddress, hostAddress)
			}
			if resp != nil {
				a.onPlacementOrder(resp)
			}
		}
	}()
}

func (a *actorsRuntime) getPlacementClientPersistently(placementAddress, hostAddress string) pb.PlacementService_ReportActionStatusClient {
	for {
		retryInterval := time.Millisecond * 250

		conn, err := grpc.Dial(placementAddress, grpc.WithInsecure())
		if err != nil {
			time.Sleep(retryInterval)
			continue
		}
		header := metadata.New(map[string]string{idHeader: hostAddress})
		ctx := metadata.NewOutgoingContext(context.Background(), header)
		client := pb.NewPlacementServiceClient(conn)
		stream, err := client.ReportActionStatus(ctx)
		if err != nil {
			time.Sleep(retryInterval)
			continue
		}

		return stream
	}
}

func (a *actorsRuntime) onPlacementOrder(in *pb.PlacementOrder) {
	log.Infof("actors: placement order received: %s", in.Operation)

	// lock all incoming calls when an updated table arrives
	a.operationUpdateLock.Lock()
	defer a.operationUpdateLock.Unlock()

	switch in.Operation {
	case lockOperation:
		{
			a.blockPlacements()

			go func() {
				time.Sleep(time.Second * 5)
				a.unblockPlacements()
			}()
		}
	case unlockOperation:
		{
			a.unblockPlacements()
		}
	case updateOperation:
		{
			a.updatePlacements(in.Tables)
		}
	}
}

func (a *actorsRuntime) blockPlacements() {
	a.placementSignal = make(chan struct{})
	a.placementBlock = true
}

func (a *actorsRuntime) unblockPlacements() {
	if a.placementBlock {
		a.placementBlock = false
		close(a.placementSignal)
	}
}

func (a *actorsRuntime) updatePlacements(in *pb.PlacementTables) {
	if in.Version != a.placementTables.Version {
		for k, v := range in.Entries {
			loadMap := map[string]*placement.Host{}
			for lk, lv := range v.LoadMap {
				loadMap[lk] = placement.NewHost(lv.Name, lv.Load, lv.Port)
			}
			c := placement.NewFromExisting(v.Hosts, v.SortedSet, loadMap)
			a.placementTables.Entries[k] = c
		}

		a.placementTables.Version = in.Version
		log.Info("actors: placement tables updated")

		go a.evaluateReminders()
	}
}

func (a *actorsRuntime) evaluateReminders() {
	a.evaluationLock.Lock()
	defer a.evaluationLock.Unlock()

	a.evaluationBusy = true
	a.evaluationChan = make(chan bool)

	var wg sync.WaitGroup
	for _, t := range a.config.HostedActorTypes {
		vals, err := a.getRemindersForActorType(t)
		if err != nil {
			log.Debugf("error getting reminders for actor type %s: %s", t, err)
		} else {
			a.remindersLock.Lock()
			a.reminders[t] = vals
			a.remindersLock.Unlock()

			go func(wg *sync.WaitGroup, reminders []Reminder) {
				wg.Add(1)
				defer wg.Done()

				for _, r := range reminders {
					targetActorAddress := a.lookupActorAddress(r.ActorType, r.ActorID)
					if targetActorAddress == "" {
						continue
					}

					if a.isActorLocal(targetActorAddress, a.config.HostAddress, a.config.Port) {
						actorKey := a.constructCombinedActorKey(r.ActorType, r.ActorID)
						reminderKey := fmt.Sprintf("%s-%s", actorKey, r.Name)
						_, exists := a.activeReminders.Load(reminderKey)

						if !exists {
							err := a.startReminder(&r)
							if err != nil {
								log.Debugf("error starting reminder: %s", err)
							}
						}
					}
				}
			}(&wg, vals)
		}
	}
	wg.Wait()
	close(a.evaluationChan)
	a.evaluationBusy = false
}

func (a *actorsRuntime) lookupActorAddress(actorType, actorID string) string {
	// read lock for table map
	a.placementTableLock.RLock()
	defer a.placementTableLock.RUnlock()

	t := a.placementTables.Entries[actorType]
	if t == nil {
		return ""
	}
	host, err := t.GetHost(actorID)
	if err != nil || host == nil {
		return ""
	}
	return fmt.Sprintf("%s:%v", host.Name, host.Port)
}

func (a *actorsRuntime) getReminderTrack(actorKey, name string) (*ReminderTrack, error) {
	resp, err := a.store.Get(&state.GetRequest{
		Key: fmt.Sprintf("%s-%s", actorKey, name),
	})
	if err != nil {
		return nil, err
	}

	var track ReminderTrack
	json.Unmarshal(resp.Data, &track)
	return &track, nil
}

func (a *actorsRuntime) updateReminderTrack(actorKey, name string) error {
	track := ReminderTrack{
		LastFiredTime: time.Now().UTC().Format(time.RFC3339),
	}

	err := a.store.Set(&state.SetRequest{
		Key:   fmt.Sprintf("%s-%s", actorKey, name),
		Value: track,
	})
	return err
}

func (a *actorsRuntime) getUpcomingReminderInvokeTime(reminder *Reminder) (time.Time, error) {
	var nextInvokeTime time.Time

	registeredTime, err := time.Parse(time.RFC3339, reminder.RegisteredTime)
	if err != nil {
		return nextInvokeTime, fmt.Errorf("error parsing reminder registered time: %s", err)
	}

	dueTime, err := time.ParseDuration(reminder.DueTime)
	if err != nil {
		return nextInvokeTime, fmt.Errorf("error parsing reminder due time: %s", err)
	}

	key := a.constructCombinedActorKey(reminder.ActorType, reminder.ActorID)
	track, err := a.getReminderTrack(key, reminder.Name)
	if err != nil {
		return nextInvokeTime, fmt.Errorf("error getting reminder track: %s", err)
	}

	var lastFiredTime time.Time
	if track != nil && track.LastFiredTime != "" {
		lastFiredTime, err = time.Parse(time.RFC3339, track.LastFiredTime)
		if err != nil {
			return nextInvokeTime, fmt.Errorf("error parsing reminder last fired time: %s", err)
		}
	}

	if reminder.Period != "" {
		period, err := time.ParseDuration(reminder.Period)
		if err != nil {
			return nextInvokeTime, fmt.Errorf("error parsing reminder period: %s", err)
		}

		if !lastFiredTime.IsZero() {
			nextInvokeTime = lastFiredTime.Add(period)
		} else {
			nextInvokeTime = registeredTime.Add(dueTime)
		}
	} else {
		if !lastFiredTime.IsZero() {
			nextInvokeTime = lastFiredTime.Add(dueTime)
		} else {
			nextInvokeTime = registeredTime.Add(dueTime)
		}
	}

	return nextInvokeTime, nil
}

func (a *actorsRuntime) startReminder(reminder *Reminder) error {
	actorKey := a.constructCombinedActorKey(reminder.ActorType, reminder.ActorID)
	reminderKey := fmt.Sprintf("%s-%s", actorKey, reminder.Name)
	nextInvokeTime, err := a.getUpcomingReminderInvokeTime(reminder)
	if err != nil {
		return err
	}

	go func() {
		now := time.Now().UTC()
		initialDuration := nextInvokeTime.Sub(now)
		time.Sleep(initialDuration)
		err = a.executeReminder(reminder.ActorType, reminder.ActorID, reminder.DueTime, reminder.Period, reminder.Name, reminder.Data)
		if err != nil {
			log.Errorf("error executing reminder: %s", err)
		}

		if reminder.Period != "" {
			period, err := time.ParseDuration(reminder.Period)
			if err != nil {
				log.Errorf("error parsing reminder period: %s", err)
			}

			stop := make(chan bool, 1)
			a.activeReminders.Store(reminderKey, stop)

			t := time.NewTicker(period)
			go func(ticker *time.Ticker, stop chan (bool), actorType, actorID, reminder, dueTime, period string, data interface{}) {
				for {
					select {
					case <-ticker.C:
						err := a.executeReminder(actorType, actorID, dueTime, period, reminder, data)
						if err != nil {
							log.Debugf("error invoking reminder on actor %s: %s", a.constructCombinedActorKey(actorType, actorID), err)
						}
					case <-stop:
						return
					}
				}
			}(t, stop, reminder.ActorType, reminder.ActorID, reminder.Name, reminder.DueTime, reminder.Period, reminder.Data)
		} else {
			err := a.DeleteReminder(&DeleteReminderRequest{
				Name:      reminder.Name,
				ActorID:   reminder.ActorID,
				ActorType: reminder.ActorType,
			})
			if err != nil {
				log.Errorf("error deleting reminder: %s", err)
			}
		}
	}()

	return nil
}

func (a *actorsRuntime) executeReminder(actorType, actorID, dueTime, period, reminder string, data interface{}) error {
	r := ReminderResponse{
		DueTime: dueTime,
		Period:  period,
		Data:    data,
	}
	b, err := json.Marshal(&r)
	if err != nil {
		return err
	}

	_, err = a.callLocalActor(actorType, actorID, fmt.Sprintf("remind/%s", reminder), b, nil)
	if err == nil {
		key := a.constructCombinedActorKey(actorType, actorID)
		a.updateReminderTrack(key, reminder)
	}
	return err
}

func (a *actorsRuntime) reminderRequiresUpdate(req *CreateReminderRequest, reminder *Reminder) bool {
	if reminder.ActorID == req.ActorID && reminder.ActorType == req.ActorType && reminder.Name == req.Name &&
		(reminder.Data != req.Data || reminder.DueTime != req.DueTime || reminder.Period != req.Period) {
		return true
	}

	return false
}

func (a *actorsRuntime) getReminder(req *CreateReminderRequest) (*Reminder, bool) {
	a.remindersLock.RLock()
	reminders := a.reminders[req.ActorType]
	a.remindersLock.RUnlock()

	for _, r := range reminders {
		if r.ActorID == req.ActorID && r.ActorType == req.ActorType && r.Name == req.Name {
			return &r, true
		}
	}

	return nil, false
}

func (a *actorsRuntime) CreateReminder(req *CreateReminderRequest) error {
	r, exists := a.getReminder(req)
	if exists {
		if a.reminderRequiresUpdate(req, r) {
			err := a.DeleteReminder(&DeleteReminderRequest{
				ActorID:   req.ActorID,
				ActorType: req.ActorType,
				Name:      req.Name,
			})
			if err != nil {
				return err
			}
		} else {
			return nil
		}
	}

	if a.evaluationBusy {
		for {
			select {
			case <-time.After(time.Second * 5):
				return errors.New("error creating reminder: timed out after 5s")
			case <-a.evaluationChan:
				break
			}
		}
	}

	reminder := Reminder{
		ActorID:        req.ActorID,
		ActorType:      req.ActorType,
		Name:           req.Name,
		Data:           req.Data,
		Period:         req.Period,
		DueTime:        req.DueTime,
		RegisteredTime: time.Now().UTC().Format(time.RFC3339),
	}

	reminders, err := a.getRemindersForActorType(req.ActorType)
	if err != nil {
		return err
	}

	reminders = append(reminders, reminder)

	err = a.store.Set(&state.SetRequest{
		Key:   fmt.Sprintf("actors-%s", req.ActorType),
		Value: reminders,
	})
	if err != nil {
		return err
	}

	a.remindersLock.Lock()
	a.reminders[req.ActorType] = reminders
	a.remindersLock.Unlock()

	err = a.startReminder(&reminder)
	if err != nil {
		return err
	}
	return nil
}

func (a *actorsRuntime) CreateTimer(req *CreateTimerRequest) error {
	actorKey := a.constructCombinedActorKey(req.ActorType, req.ActorID)
	timerKey := fmt.Sprintf("%s-%s", actorKey, req.Name)

	_, exists := a.actorsTable.Load(actorKey)
	if !exists {
		return fmt.Errorf("can't create timer for actor %s: actor not activated", actorKey)
	}

	stopChan, exists := a.activeTimers.Load(timerKey)
	if exists {
		close(stopChan.(chan bool))
	}

	d, err := time.ParseDuration(req.Period)
	if err != nil {
		return err
	}

	t := time.NewTicker(d)
	stop := make(chan bool, 1)
	a.activeTimers.Store(timerKey, stop)

	go func(ticker *time.Ticker, stop chan (bool), actorType, actorID, name, dueTime, period, callback string, data interface{}) {
		if dueTime != "" {
			d, err := time.ParseDuration(dueTime)
			if err == nil {
				time.Sleep(d)
			}
		}

		actorKey := a.constructCombinedActorKey(actorType, actorID)

		for {
			select {
			case <-ticker.C:
				_, exists := a.actorsTable.Load(actorKey)
				if exists {
					err := a.executeTimer(actorType, actorID, name, dueTime, period, callback, data)
					if err != nil {
						log.Debugf("error invoking timer on actor %s: %s", actorKey, err)
					}
				} else {
					a.DeleteTimer(&DeleteTimerRequest{
						Name:      name,
						ActorID:   actorID,
						ActorType: actorType,
					})
				}
			case <-stop:
				return
			}
		}
	}(t, stop, req.ActorType, req.ActorID, req.Name, req.DueTime, req.Period, req.Callback, req.Data)
	return nil
}

func (a *actorsRuntime) executeTimer(actorType, actorID, name, dueTime, period, callback string, data interface{}) error {
	t := TimerResponse{
		Callback: callback,
		Data:     data,
		DueTime:  dueTime,
		Period:   period,
	}
	b, err := json.Marshal(&t)
	if err != nil {
		return err
	}
	_, err = a.callLocalActor(actorType, actorID, fmt.Sprintf("timer/%s", name), b, nil)
	return err
}

func (a *actorsRuntime) getRemindersForActorType(actorType string) ([]Reminder, error) {
	key := fmt.Sprintf("actors-%s", actorType)
	resp, err := a.store.Get(&state.GetRequest{
		Key: key,
	})
	if err != nil {
		return nil, err
	}

	var reminders []Reminder
	json.Unmarshal(resp.Data, &reminders)
	return reminders, nil
}

func (a *actorsRuntime) DeleteReminder(req *DeleteReminderRequest) error {
	if a.evaluationBusy {
		for {
			select {
			case <-time.After(time.Second * 5):
				return errors.New("error creating reminder: timed out after 5s")
			case <-a.evaluationChan:
				break
			}
		}
	}

	key := fmt.Sprintf("actors-%s", req.ActorType)
	reminders, err := a.getRemindersForActorType(req.ActorType)
	if err != nil {
		return err
	}

	for i := len(reminders) - 1; i >= 0; i-- {
		if reminders[i].ActorType == req.ActorType && reminders[i].ActorID == req.ActorID && reminders[i].Name == req.Name {
			reminders = append(reminders[:i], reminders[i+1:]...)
		}
	}

	actorKey := a.constructCombinedActorKey(req.ActorType, req.ActorID)
	reminderKey := fmt.Sprintf("%s-%s", actorKey, req.Name)

	stopChan, exists := a.activeReminders.Load(reminderKey)
	if exists {
		close(stopChan.(chan bool))
		a.activeReminders.Delete(reminderKey)
	}

	err = a.store.Set(&state.SetRequest{
		Key:   key,
		Value: reminders,
	})
	if err != nil {
		return err
	}

	a.remindersLock.Lock()
	a.reminders[req.ActorType] = reminders
	a.remindersLock.Unlock()

	err = a.store.Delete(&state.DeleteRequest{
		Key: reminderKey,
	})
	if err != nil {
		return err
	}

	return nil
}

func (a *actorsRuntime) DeleteTimer(req *DeleteTimerRequest) error {
	actorKey := a.constructCombinedActorKey(req.ActorType, req.ActorID)
	timerKey := fmt.Sprintf("%s-%s", actorKey, req.Name)

	stopChan, exists := a.activeTimers.Load(timerKey)
	if exists {
		close(stopChan.(chan bool))
		a.activeTimers.Delete(timerKey)
	}

	return nil
}
