// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sdkserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/mennanov/fmutils"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	k8sv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"

	agonesv1 "agones.dev/agones/pkg/apis/agones/v1"
	"agones.dev/agones/pkg/client/clientset/versioned"
	typedv1 "agones.dev/agones/pkg/client/clientset/versioned/typed/agones/v1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listersv1 "agones.dev/agones/pkg/client/listers/agones/v1"
	"agones.dev/agones/pkg/sdk"
	"agones.dev/agones/pkg/sdk/alpha"
	"agones.dev/agones/pkg/sdk/beta"
	"agones.dev/agones/pkg/sdkserver/mutator"
	"agones.dev/agones/pkg/util/apiserver"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/workerqueue"
)

var (
	_ sdk.SDKServer   = &SDKServer{}
	_ alpha.SDKServer = &SDKServer{}
	_ beta.SDKServer  = &SDKServer{}
)

// SDKServer is a gRPC server, that is meant to be a sidecar
// for a GameServer that will update the game server status on SDK requests
//
//nolint:govet // ignore fieldalignment, singleton
type SDKServer struct {
	logger              *logrus.Entry
	gameServerName      string
	namespace           string
	informerFactory     externalversions.SharedInformerFactory
	gameServerGetter    typedv1.GameServersGetter
	gameServerLister    listersv1.GameServerLister
	gameServerSynced    cache.InformerSynced
	connected           bool
	server              *http.Server
	clock               clock.Clock
	health              agonesv1.Health
	healthTimeout       time.Duration
	healthMutex         sync.RWMutex
	healthLastUpdated   time.Time
	healthFailureCount  int32
	healthChecksRunning sync.Once
	streamMutex         sync.RWMutex
	connectedStreams    []sdk.SDK_WatchGameServerServer
	ctx                 context.Context
	recorder            record.EventRecorder
	mutator             *mutator.Mutator[agonesv1.GameServer]
	gsUpdateMutex       sync.RWMutex
	gsWaitForSync       sync.WaitGroup
	reserveTimer        *time.Timer
	gsReserveDuration   *time.Duration
}

// NewSDKServer creates a SDKServer that sets up an
// InClusterConfig for Kubernetes
func NewSDKServer(gameServerName, namespace string, kubeClient kubernetes.Interface,
	agonesClient versioned.Interface, logLevel logrus.Level, healthPort int, requestsRateLimit time.Duration) (*SDKServer, error) {
	mux := http.NewServeMux()
	resync := 0 * time.Second

	// limit the informer to only working with the gameserver that the sdk is attached to
	tweakListOptions := func(opts *metav1.ListOptions) {
		s1 := fields.OneTermEqualSelector("metadata.name", gameServerName)
		opts.FieldSelector = s1.String()
	}
	factory := externalversions.NewSharedInformerFactoryWithOptions(agonesClient, resync, externalversions.WithNamespace(namespace), externalversions.WithTweakListOptions(tweakListOptions))
	gameServers := factory.Agones().V1().GameServers()

	s := &SDKServer{
		gameServerName:   gameServerName,
		namespace:        namespace,
		gameServerGetter: agonesClient.AgonesV1(),
		gameServerLister: gameServers.Lister(),
		gameServerSynced: gameServers.Informer().HasSynced,
		server: &http.Server{
			Addr:    fmt.Sprintf(":%d", healthPort),
			Handler: mux,
		},
		clock:              clock.RealClock{},
		healthMutex:        sync.RWMutex{},
		healthFailureCount: 0,
		streamMutex:        sync.RWMutex{},
		gsUpdateMutex:      sync.RWMutex{},
		gsWaitForSync:      sync.WaitGroup{},
	}

	if runtime.FeatureEnabled(runtime.FeatureCountsAndLists) {
		// Once FeatureCountsAndLists is in GA, move this into SDKServer creation above.
		// s.gsCounterUpdates = map[string]counterUpdateRequest{}
		// s.gsListUpdates = map[string]listUpdateRequest{}
	}

	s.informerFactory = factory
	s.logger = runtime.NewLoggerWithType(s).WithField("gsKey", namespace+"/"+gameServerName)
	s.logger.Logger.SetLevel(logLevel)

	_, _ = gameServers.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(_, newObj interface{}) {
			gs := newObj.(*agonesv1.GameServer)
			s.sendGameServerUpdate(gs)
		},
	})

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(s.logger.Debugf)
	eventBroadcaster.StartRecordingToSink(&k8sv1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	s.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "gameserver-sidecar"})

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, err := w.Write([]byte("ok"))
		if err != nil {
			s.logger.WithError(err).Error("could not send ok response on healthz")
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/gshealthz", func(w http.ResponseWriter, _ *http.Request) {
		s.ensureHealthChecksRunning()
		if s.healthy() {
			_, err := w.Write([]byte("ok"))
			if err != nil {
				s.logger.WithError(err).Error("could not send ok response on gshealthz")
				w.WriteHeader(http.StatusInternalServerError)
			}
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	s.mutator = mutator.NewMutator[agonesv1.GameServer](
		50*time.Millisecond,
		500*time.Millisecond,
		s.flushGameServer,
	)

	// we haven't synced yet
	s.gsWaitForSync.Add(1)
	s.logger.Info("Created GameServer sidecar")

	return s, nil
}

// Run processes the rate limited queue.
// Will block until stop is closed
func (s *SDKServer) Run(ctx context.Context) error {

	defer func() {
		if r := recover(); r != nil {
			s.logger.WithField("panic", r).Error("Panic in SDKServer.Run")
		}
	}()

	s.logger.Info("Run(): Starting informer factory")
	s.informerFactory.Start(ctx.Done())
	s.logger.Info("Run(): Waiting for cache sync")
	if !cache.WaitForCacheSync(ctx.Done(), s.gameServerSynced) {
		s.logger.Error("Run(): failed to wait for caches to sync")
		return errors.New("failed to wait for caches to sync")
	}
	s.logger.Info("Run(): Cache sync complete")

	// need this for streaming gRPC commands
	s.ctx = ctx
	// we have the gameserver details now
	s.logger.Info("Run(): Calling gsWaitForSync.Done()")
	s.gsWaitForSync.Done()

	s.logger.Info("before gameServer")
	gs, err := s.gameServer()
	if err != nil {
		s.logger.Info("error gameServer")
		return err
	}
	s.logger.Info("after gameServer")

	s.health = gs.Spec.Health
	s.logger.WithField("health", s.health).Info("Setting health configuration")
	s.healthTimeout = time.Duration(gs.Spec.Health.PeriodSeconds) * time.Second
	s.touchHealthLastUpdated()

	if gs.Status.State == agonesv1.GameServerStateReserved && gs.Status.ReservedUntil != nil {
		s.gsUpdateMutex.Lock()
		s.resetReserveAfter(context.Background(), time.Until(gs.Status.ReservedUntil.Time))
		s.gsUpdateMutex.Unlock()
	}

	// then start the http endpoints
	s.logger.Info("Starting SDKServer http health check...")
	go func() {
		if err := s.server.ListenAndServe(); err != nil {
			if err == http.ErrServerClosed {
				s.logger.WithError(err).Error("Health check: http server closed")
			} else {
				err = errors.Wrap(err, "Could not listen on :8080")
				runtime.HandleError(s.logger.WithError(err), err)
			}
		}
	}()
	defer s.server.Close() // nolint: errcheck

	s.logger.Info("before mutator run")
	s.mutator.Run(ctx)

	s.logger.Info("after mutator run")

	return nil
}

// WaitForConnection attempts a GameServer GET every 3s until the client responds.
// This is a workaround for the informer hanging indefinitely on first LIST due
// to a flaky network to the Kubernetes service endpoint.
func (s *SDKServer) WaitForConnection(ctx context.Context) error {
	// In normal operaiton, waitForConnection is called exactly once in Run().
	// In unit tests, waitForConnection() can be called before Run() to ensure
	// that connected is true when Run() is called, otherwise the List() below
	// may race with a test that changes a mock. (Despite the fact that we drop
	// the data on the ground, the Go race detector will pereceive a data race.)
	if s.connected {
		return nil
	}

	try := 0
	return wait.PollUntilContextCancel(ctx, 4*time.Second, true, func(ctx context.Context) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		// Specifically use gameServerGetter since it's the raw client (gameServerLister is the informer).
		// We use List here to avoid needing permission to Get().
		_, err := s.gameServerGetter.GameServers(s.namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", s.gameServerName).String(),
		})
		if err != nil {
			s.logger.WithField("try", try).WithError(err).Info("Connection to Kubernetes service failed")
			try++
			return false, nil
		}
		s.logger.WithField("try", try).Info("Connection to Kubernetes service established")
		s.connected = true
		return true, nil
	})
}

// Gets the GameServer from the cache, or from the local SDKServer if that version is more recent.
func (s *SDKServer) gameServer() (*agonesv1.GameServer, error) {
	s.logger.Debug("gameServer(): Waiting for gsWaitForSync")
	s.gsWaitForSync.Wait()
	s.logger.Debug("gameServer(): gsWaitForSync released, fetching GameServer from lister")
	gs, err := s.gameServerLister.GameServers(s.namespace).Get(s.gameServerName)
	if err != nil {
		s.logger.WithError(err).Error("gameServer(): could not retrieve GameServer from lister")
		return gs, errors.Wrapf(err, "could not retrieve GameServer %s/%s", s.namespace, s.gameServerName)
	}
	s.logger.Debug("gameServer(): Returning GameServer from lister")
	return gs, nil
}

// patchGameServer is a helper function to create and apply a patch update, so the changes in
// gsCopy are applied to the original gs.
func (s *SDKServer) patchGameServer(ctx context.Context, gs, gsCopy *agonesv1.GameServer) (*agonesv1.GameServer, error) {
	patch, err := gs.Patch(gsCopy)
	if err != nil {
		return nil, err
	}

	gs, err = s.gameServerGetter.GameServers(s.namespace).Patch(ctx, gs.GetObjectMeta().GetName(), types.JSONPatchType, patch, metav1.PatchOptions{})
	// if the test operation fails, no reason to error log
	if err != nil && k8serrors.IsInvalid(err) {
		err = workerqueue.NewTraceError(err)
	}
	return gs, errors.Wrapf(err, "error attempting to patch gameserver: %s/%s", gsCopy.ObjectMeta.Namespace, gsCopy.ObjectMeta.Name)
}

// Ready enters the RequestReady state change for this GameServer into
// the workqueue so it can be updated
func (s *SDKServer) Ready(_ context.Context, e *sdk.Empty) (*sdk.Empty, error) {
	s.logger.Debug("Received Ready request")
	s.stopReserveTimer()
	resultCh := make(chan error, 1)
	action := &mutator.SetStateAction{
		NewState: agonesv1.GameServerStateRequestReady,
		Recorder: s.recorder,
		Result:   resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(action.ErrCh()); err != nil {
		return nil, err
	}

	return e, nil
}

// Allocate enters an Allocate state change into the workqueue, so it can be updated
func (s *SDKServer) Allocate(_ context.Context, e *sdk.Empty) (*sdk.Empty, error) {
	s.stopReserveTimer()
	resultCh := make(chan error, 1)
	action := &mutator.SetStateAction{
		NewState: agonesv1.GameServerStateAllocated,
		Result:   resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(action.ErrCh()); err != nil {
		return nil, err
	}
	return e, nil
}

// Shutdown enters the Shutdown state change for this GameServer into
// the workqueue so it can be updated. If gracefulTermination feature is enabled,
// Shutdown will block on GameServer being shutdown.
func (s *SDKServer) Shutdown(_ context.Context, e *sdk.Empty) (*sdk.Empty, error) {
	s.logger.Debug("Received Shutdown request, adding to queue")
	s.stopReserveTimer()
	// s.enqueueState(agonesv1.GameServerStateShutdown)

	return e, nil
}

// Health receives each health ping, and tracks the last time the health
// check was received, to track if a GameServer is healthy
func (s *SDKServer) Health(stream sdk.SDK_HealthServer) error {
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			s.logger.Debug("Health stream closed.")
			return stream.SendAndClose(&sdk.Empty{})
		}
		if err != nil {
			return errors.Wrap(err, "Error with Health check")
		}
		s.logger.Debug("Health Ping Received")
		s.touchHealthLastUpdated()
	}
}

// SetLabel adds the Key/Value to be used to set the label with the metadataPrefix to the `GameServer`
// metdata
func (s *SDKServer) SetLabel(_ context.Context, kv *sdk.KeyValue) (*sdk.Empty, error) {
	resultCh := make(chan error, 1)
	action := &mutator.SetLabelAction{
		Key:    kv.Key,
		Value:  kv.Value,
		Result: resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(action.ErrCh()); err != nil {
		return nil, err
	}

	return &sdk.Empty{}, nil
}

// SetAnnotation adds the Key/Value to be used to set the annotations with the metadataPrefix to the `GameServer`
// metdata
func (s *SDKServer) SetAnnotation(_ context.Context, kv *sdk.KeyValue) (*sdk.Empty, error) {
	resultCh := make(chan error, 1)
	action := &mutator.SetAnnotationAction{
		Key:    kv.Key,
		Value:  kv.Value,
		Result: resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(action.ErrCh()); err != nil {
		return nil, err
	}
	return &sdk.Empty{}, nil
}

// GetGameServer returns the current GameServer configuration and state from the backing GameServer CRD
func (s *SDKServer) GetGameServer(context.Context, *sdk.Empty) (*sdk.GameServer, error) {
	s.logger.Debug("Received GetGameServer request")
	gs, err := s.gameServer()
	if err != nil {
		return nil, err
	}
	return convert(gs), nil
}

// WatchGameServer sends events through the stream when changes occur to the
// backing GameServer configuration / status
func (s *SDKServer) WatchGameServer(_ *sdk.Empty, stream sdk.SDK_WatchGameServerServer) error {
	s.logger.Debug("Received WatchGameServer request, adding stream to connectedStreams")

	gs, err := s.GetGameServer(context.Background(), &sdk.Empty{})
	if err != nil {
		return err
	}

	if err := stream.Send(gs); err != nil {
		return err
	}

	s.streamMutex.Lock()
	s.connectedStreams = append(s.connectedStreams, stream)
	s.streamMutex.Unlock()
	// don't exit until we shutdown, because that will close the stream
	<-s.ctx.Done()
	return nil
}

// Reserve moves this GameServer to the Reserved state for the Duration specified
func (s *SDKServer) Reserve(_ context.Context, d *sdk.Duration) (*sdk.Empty, error) {
	s.stopReserveTimer()
	var duration *time.Duration
	if d.Seconds > 0 {
		dur := time.Duration(d.Seconds) * time.Second
		duration = &dur
	}
	resultCh := make(chan error, 1)
	action := &mutator.SetStateAction{
		NewState:        agonesv1.GameServerStateReserved,
		ReserveDuration: duration,
		Recorder:        s.recorder,
		ReserveTimer: func(d time.Duration) {
			s.resetReserveAfter(context.Background(), d)
		},
		Result: resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(action.ErrCh()); err != nil {
		return nil, err
	}

	return &sdk.Empty{}, nil
}

// resetReserveAfter will move the GameServer back to being ready after the specified duration.
// This function should be wrapped in a s.gsUpdateMutex lock when being called.
func (s *SDKServer) resetReserveAfter(ctx context.Context, duration time.Duration) {
	if s.reserveTimer != nil {
		s.reserveTimer.Stop()
	}

	s.reserveTimer = time.AfterFunc(duration, func() {
		if _, err := s.Ready(ctx, &sdk.Empty{}); err != nil {
			s.logger.WithError(errors.WithStack(err)).Error("error returning to Ready after reserved")
		}
	})
}

// stopReserveTimer stops the reserve timer. This is a no-op and safe to call if the timer is nil
func (s *SDKServer) stopReserveTimer() {
	s.gsUpdateMutex.Lock()
	defer s.gsUpdateMutex.Unlock()

	if s.reserveTimer != nil {
		s.reserveTimer.Stop()
	}
	s.gsReserveDuration = nil
}

// PlayerConnect should be called when a player connects.
// [Stage:Alpha]
// [FeatureFlag:PlayerTracking]
func (s *SDKServer) PlayerConnect(_ context.Context, id *alpha.PlayerID) (*alpha.Bool, error) {
	resultCh := make(chan error, 1)
	action := &mutator.PlayerConnectAction{
		PlayerID: id.PlayerID,
		Result:   resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(resultCh); err != nil {
		return &alpha.Bool{Bool: false}, err
	}
	return &alpha.Bool{Bool: true}, nil
}

// PlayerDisconnect should be called when a player disconnects.
// [Stage:Alpha]
// [FeatureFlag:PlayerTracking]
func (s *SDKServer) PlayerDisconnect(_ context.Context, id *alpha.PlayerID) (*alpha.Bool, error) {
	resultCh := make(chan error, 1)
	action := &mutator.PlayerDisconnectAction{
		PlayerID: id.PlayerID,
		Result:   resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(action.ErrCh()); err != nil {
		return nil, err
	}

	return &alpha.Bool{Bool: true}, nil
}

// IsPlayerConnected returns if the playerID is currently connected to the GameServer.
// This is always accurate, even if the value hasn’t been updated to the GameServer status yet.
// [Stage:Alpha]
// [FeatureFlag:PlayerTracking]
func (s *SDKServer) IsPlayerConnected(_ context.Context, id *alpha.PlayerID) (*alpha.Bool, error) {
	if !runtime.FeatureEnabled(runtime.FeaturePlayerTracking) {
		return &alpha.Bool{Bool: false}, errors.Errorf("%s not enabled", runtime.FeaturePlayerTracking)
	}
	gs, err := s.gameServer()
	if err != nil {
		return &alpha.Bool{Bool: false}, err
	}
	result := &alpha.Bool{Bool: false}
	if gs.Status.Players != nil {
		for _, playerID := range gs.Status.Players.IDs {
			if playerID == id.PlayerID {
				result.Bool = true
				break
			}
		}
	}
	return result, nil
}

// GetConnectedPlayers returns the list of the currently connected player ids.
// This is always accurate, even if the value hasn’t been updated to the GameServer status yet.
// [Stage:Alpha]
// [FeatureFlag:PlayerTracking]
func (s *SDKServer) GetConnectedPlayers(_ context.Context, _ *alpha.Empty) (*alpha.PlayerIDList, error) {
	if !runtime.FeatureEnabled(runtime.FeaturePlayerTracking) {
		return nil, errors.Errorf("%s not enabled", runtime.FeaturePlayerTracking)
	}
	gs, err := s.gameServer()
	if err != nil {
		return nil, err
	}
	if gs.Status.Players == nil {
		return &alpha.PlayerIDList{List: []string{}}, nil
	}
	return &alpha.PlayerIDList{List: gs.Status.Players.IDs}, nil
}

// GetPlayerCount returns the current player count.
// [Stage:Alpha]
// [FeatureFlag:PlayerTracking]
func (s *SDKServer) GetPlayerCount(_ context.Context, _ *alpha.Empty) (*alpha.Count, error) {
	if !runtime.FeatureEnabled(runtime.FeaturePlayerTracking) {
		return nil, errors.Errorf("%s not enabled", runtime.FeaturePlayerTracking)
	}
	gs, err := s.gameServer()
	if err != nil {
		return nil, err
	}
	count := int64(0)
	if gs.Status.Players != nil {
		count = int64(len(gs.Status.Players.IDs))
	}
	return &alpha.Count{Count: count}, nil
}

// SetPlayerCapacity to change the game server's player capacity.
// [Stage:Alpha]
// [FeatureFlag:PlayerTracking]
func (s *SDKServer) SetPlayerCapacity(_ context.Context, count *alpha.Count) (*alpha.Empty, error) {
	resultCh := make(chan error, 1)
	action := &mutator.SetPlayerCapacityAction{
		NewCapacity: count.Count,
		Result:      resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(resultCh); err != nil {
		return nil, err
	}
	return &alpha.Empty{}, nil
}

// GetPlayerCapacity returns the current player capacity, as set by SDK.SetPlayerCapacity()
// [Stage:Alpha]
// [FeatureFlag:PlayerTracking]
func (s *SDKServer) GetPlayerCapacity(_ context.Context, _ *alpha.Empty) (*alpha.Count, error) {
	if !runtime.FeatureEnabled(runtime.FeaturePlayerTracking) {
		return nil, errors.Errorf("%s not enabled", runtime.FeaturePlayerTracking)
	}
	gs, err := s.gameServer()
	if err != nil {
		return nil, err
	}
	capacity := int64(0)
	if gs.Status.Players != nil {
		capacity = gs.Status.Players.Capacity
	}
	return &alpha.Count{Count: capacity}, nil
}

// GetCounter returns a Counter. Returns error if the counter does not exist.
// [Stage:Beta]
// [FeatureFlag:CountsAndLists]
func (s *SDKServer) GetCounter(_ context.Context, in *beta.GetCounterRequest) (*beta.Counter, error) {
	if !runtime.FeatureEnabled(runtime.FeatureCountsAndLists) {
		return nil, errors.Errorf("%s not enabled", runtime.FeatureCountsAndLists)
	}
	gs, err := s.gameServer()
	if err != nil {
		return nil, err
	}
	counter, ok := gs.Status.Counters[in.Name]
	if !ok {
		return nil, errors.Errorf("counter not found: %s", in.Name)
	}
	return &beta.Counter{Name: in.Name, Count: counter.Count, Capacity: counter.Capacity}, nil
}

// UpdateCounter collapses all UpdateCounterRequests for a given Counter into a single request.
// UpdateCounterRequest must be one and only one of Capacity, Count, or CountDiff.
// Returns error if the Counter does not exist (name cannot be updated).
// Returns error if the Count is out of range [0,Capacity].
// [Stage:Beta]
// [FeatureFlag:CountsAndLists]
func (s *SDKServer) UpdateCounter(_ context.Context, in *beta.UpdateCounterRequest) (*beta.Counter, error) {
	if !runtime.FeatureEnabled(runtime.FeatureCountsAndLists) {
		return nil, errors.Errorf("%s not enabled", runtime.FeatureCountsAndLists)
	}

	if in.CounterUpdateRequest == nil {
		return nil, errors.Errorf("invalid argument. CounterUpdateRequest: %v cannot be nil", in.CounterUpdateRequest)
	}
	if in.CounterUpdateRequest.CountDiff == 0 && in.CounterUpdateRequest.Count == nil && in.CounterUpdateRequest.Capacity == nil {
		return nil, errors.Errorf("invalid argument. Malformed CounterUpdateRequest: %v", in.CounterUpdateRequest)
	}

	gs, err := s.gameServer()
	if err != nil {
		return nil, err
	}
	name := in.CounterUpdateRequest.Name
	counter, ok := gs.Status.Counters[name]
	if !ok {
		return nil, errors.Errorf("counter not found: %s", name)
	}

	// Validate requested changes (no max capacity, just non-negative and within current/new capacity)
	var capacityPtr *int64
	if in.CounterUpdateRequest.Capacity != nil {
		cap := in.CounterUpdateRequest.Capacity.GetValue()
		if cap < 0 {
			return nil, errors.Errorf("out of range. Capacity must be greater than or equal to 0. Found Capacity: %d", cap)
		}
		capacityPtr = &cap
	}

	var countPtr *int64
	if in.CounterUpdateRequest.Count != nil {
		cnt := in.CounterUpdateRequest.Count.GetValue()
		capacity := counter.Capacity
		if capacityPtr != nil {
			capacity = *capacityPtr
		}
		if cnt < 0 || cnt > capacity {
			return nil, errors.Errorf("out of range. Count must be within range [0,Capacity]. Found Count: %d, Capacity: %d", cnt, capacity)
		}
		countPtr = &cnt
	}

	// For CountDiff, check what the resulting count would be
	if in.CounterUpdateRequest.CountDiff != 0 {
		count := counter.Count
		if countPtr != nil {
			count = *countPtr
		}
		capacity := counter.Capacity
		if capacityPtr != nil {
			capacity = *capacityPtr
		}
		newCount := count + in.CounterUpdateRequest.CountDiff
		if newCount < 0 || newCount > capacity {
			return nil, errors.Errorf("out of range. Count must be within range [0,Capacity]. Found Count: %d, Capacity: %d", newCount, capacity)
		}
	}

	resultCh := make(chan error, 1)
	action := &mutator.UpdateCounterAction{
		Name:      name,
		Capacity:  capacityPtr,
		Count:     countPtr,
		CountDiff: in.CounterUpdateRequest.CountDiff,
		Recorder:  s.recorder,
		Result:    resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(resultCh); err != nil {
		return nil, err
	}

	// Return the updated counter from the CRD
	gs, err = s.gameServer()
	if err != nil {
		return nil, err
	}
	updated, ok := gs.Status.Counters[name]
	if !ok {
		return nil, errors.Errorf("counter not found after update: %s", name)
	}
	return &beta.Counter{Name: name, Count: updated.Count, Capacity: updated.Capacity}, nil
}

// GetList returns a List. Returns not found if the List does not exist.
// [Stage:Beta]
// [FeatureFlag:CountsAndLists]
func (s *SDKServer) GetList(_ context.Context, in *beta.GetListRequest) (*beta.List, error) {
	if !runtime.FeatureEnabled(runtime.FeatureCountsAndLists) {
		return nil, errors.Errorf("%s not enabled", runtime.FeatureCountsAndLists)
	}
	if in == nil {
		return nil, errors.Errorf("GetListRequest cannot be nil")
	}
	s.logger.WithField("name", in.Name).Debug("Getting List")

	gs, err := s.gameServer()
	if err != nil {
		return nil, err
	}

	list, ok := gs.Status.Lists[in.Name]
	if !ok {
		return nil, errors.Errorf("list not found: %s", in.Name)
	}

	s.logger.WithField("Get List", list).Debugf("Got List %s", in.Name)
	return &beta.List{Name: in.Name, Values: list.Values, Capacity: list.Capacity}, nil
}

// UpdateList collapses all update capacity requests for a given List into a single UpdateList request.
// This function currently only updates the Capacity of a List.
// Returns error if the List does not exist (name cannot be updated).
// Returns error if the List update capacity is out of range [0,1000].
// [Stage:Beta]
// [FeatureFlag:CountsAndLists]
func (s *SDKServer) UpdateList(ctx context.Context, in *beta.UpdateListRequest) (*beta.List, error) {
	if !runtime.FeatureEnabled(runtime.FeatureCountsAndLists) {
		return nil, errors.Errorf("%s not enabled", runtime.FeatureCountsAndLists)
	}
	if in == nil {
		return nil, errors.Errorf("UpdateListRequest cannot be nil")
	}
	if in.List == nil || in.UpdateMask == nil {
		return nil, errors.Errorf("invalid argument. List: %v and UpdateMask %v cannot be nil", in.List, in.UpdateMask)
	}
	if !in.UpdateMask.IsValid(in.List.ProtoReflect().Interface()) {
		return nil, errors.Errorf("invalid argument. Field Mask Path(s): %v are invalid for List. Use valid field name(s): %v", in.UpdateMask.GetPaths(), in.List.ProtoReflect().Descriptor().Fields())
	}
	if in.List.Capacity < 0 || in.List.Capacity > apiserver.ListMaxCapacity {
		return nil, errors.Errorf("out of range. Capacity must be within range [0,1000]. Found Capacity: %d", in.List.Capacity)
	}

	// Validate list exists
	_, err := s.GetList(ctx, &beta.GetListRequest{Name: in.List.Name})
	if err != nil {
		return nil, errors.Errorf("not found. %s List not found", in.List.Name)
	}

	fmutils.Filter(in.List, in.UpdateMask.Paths)

	resultCh := make(chan error, 1)
	action := &mutator.UpdateListAction{
		Name:        in.List.Name,
		UpdateMask:  in.UpdateMask.Paths,
		NewCapacity: in.List.Capacity,
		NewValues:   in.List.Values,
		Result:      resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(resultCh); err != nil {
		return nil, err
	}
	return &beta.List{}, nil
}

// AddListValue collapses all append a value to the end of a List requests into a single UpdateList request.
// Returns not found if the List does not exist.
// Returns already exists if the value is already in the List.
// Returns out of range if the List is already at Capacity.
// [Stage:Beta]
// [FeatureFlag:CountsAndLists]
func (s *SDKServer) AddListValue(ctx context.Context, in *beta.AddListValueRequest) (*beta.List, error) {
	if !runtime.FeatureEnabled(runtime.FeatureCountsAndLists) {
		return nil, errors.Errorf("%s not enabled", runtime.FeatureCountsAndLists)
	}
	if in == nil {
		return nil, errors.Errorf("AddListValueRequest cannot be nil")
	}
	s.logger.WithField("name", in.Name).Debug("Add List Value")

	list, err := s.GetList(ctx, &beta.GetListRequest{Name: in.Name})
	if err != nil {
		return nil, err
	}
	if int(list.Capacity) <= len(list.Values) {
		return nil, errors.Errorf("out of range. No available capacity. Current Capacity: %d, List Size: %d", list.Capacity, len(list.Values))
	}
	for _, val := range list.Values {
		if in.Value == val {
			return nil, errors.Errorf("already exists. Value: %s already in List: %s", in.Value, in.Name)
		}
	}

	resultCh := make(chan error, 1)
	newValues := append(list.Values, in.Value)
	action := &mutator.UpdateListAction{
		Name:        in.Name,
		UpdateMask:  []string{"values"},
		NewCapacity: list.Capacity,
		NewValues:   newValues,
		Result:      resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(resultCh); err != nil {
		return nil, err
	}
	return &beta.List{}, nil
}

// RemoveListValue collapses all remove a value from a List requests into a single UpdateList request.
// Returns not found if the List does not exist.
// Returns not found if the value is not in the List.
// [Stage:Beta]
// [FeatureFlag:CountsAndLists]
func (s *SDKServer) RemoveListValue(ctx context.Context, in *beta.RemoveListValueRequest) (*beta.List, error) {
	if !runtime.FeatureEnabled(runtime.FeatureCountsAndLists) {
		return nil, errors.Errorf("%s not enabled", runtime.FeatureCountsAndLists)
	}
	if in == nil {
		return nil, errors.Errorf("RemoveListValueRequest cannot be nil")
	}
	s.logger.WithField("name", in.Name).Debug("Remove List Value")

	list, err := s.GetList(ctx, &beta.GetListRequest{Name: in.Name})
	if err != nil {
		return nil, err
	}
	found := false
	newValues := []string{}
	for _, val := range list.Values {
		if val == in.Value {
			found = true
			continue
		}
		newValues = append(newValues, val)
	}
	if !found {
		return nil, errors.Errorf("not found. Value: %s not found in List: %s", in.Value, in.Name)
	}

	resultCh := make(chan error, 1)
	action := &mutator.UpdateListAction{
		Name:        in.Name,
		UpdateMask:  []string{"values"},
		NewCapacity: list.Capacity,
		NewValues:   newValues,
		Result:      resultCh,
	}
	s.mutator.Push(action)
	if err := waitForErrorOrClose(resultCh); err != nil {
		return nil, err
	}
	return &beta.List{}, nil
}

// sendGameServerUpdate sends a watch game server event
func (s *SDKServer) sendGameServerUpdate(gs *agonesv1.GameServer) {
	s.logger.Debug("Sending GameServer Event to connectedStreams")

	s.streamMutex.Lock()
	defer s.streamMutex.Unlock()

	// Filter the slice of streams sharing the same backing array and capacity as the original
	// so that storage is reused and no memory allocations are made. This modifies the original
	// slice.
	//
	// See https://go.dev/wiki/SliceTricks#filtering-without-allocating
	remainingStreams := s.connectedStreams[:0]
	for _, stream := range s.connectedStreams {
		select {
		case <-stream.Context().Done():
			s.logger.Debug("Dropping stream")

			err := stream.Context().Err()
			switch {
			case err != nil:
				s.logger.WithError(errors.WithStack(err)).Error("stream closed with error")
			default:
				s.logger.Debug("Stream closed")
			}
		default:
			s.logger.Debug("Keeping stream")
			remainingStreams = append(remainingStreams, stream)

			if err := stream.Send(convert(gs)); err != nil {
				s.logger.WithError(errors.WithStack(err)).
					Error("error sending game server update event")
			}
		}
	}
	s.connectedStreams = remainingStreams
}

// checkHealthUpdateState checks the health as part of the /gshealthz hook, and if not
// healthy will push the Unhealthy state into the queue so it can be updated.
func (s *SDKServer) checkHealthUpdateState() {
	s.checkHealth()
	if !s.healthy() {
		s.logger.WithField("gameServerName", s.gameServerName).Warn("GameServer has failed health check")
		resultCh := make(chan error, 1)
		action := &mutator.SetStateAction{
			NewState: agonesv1.GameServerStateUnhealthy,
			Recorder: s.recorder,
			Result:   resultCh,
		}
		s.mutator.Push(action)
		_ = waitForErrorOrClose(action.ErrCh())
	}
}

// touchHealthLastUpdated sets the healthLastUpdated
// value to now in UTC
func (s *SDKServer) touchHealthLastUpdated() {
	s.healthMutex.Lock()
	defer s.healthMutex.Unlock()
	s.healthLastUpdated = s.clock.Now().UTC()
	s.healthFailureCount = 0
}

func (s *SDKServer) ensureHealthChecksRunning() {
	if s.health.Disabled {
		return
	}
	s.healthChecksRunning.Do(func() {
		// start health checking running
		s.logger.Debug("Starting GameServer health checking")
		go wait.Until(s.checkHealthUpdateState, s.healthTimeout, s.ctx.Done())
	})
}

// checkHealth checks the healthLastUpdated value
// and if it is outside the timeout value, logger and
// count a failure
func (s *SDKServer) checkHealth() {
	s.healthMutex.Lock()
	defer s.healthMutex.Unlock()

	timeout := s.healthLastUpdated.Add(s.healthTimeout)
	if timeout.Before(s.clock.Now().UTC()) {
		s.healthFailureCount++
		s.logger.WithField("failureCount", s.healthFailureCount).Warn("GameServer Health Check failed")
	}
}

// healthy returns if the GameServer is
// currently healthy or not based on the configured
// failure count vs failure threshold
func (s *SDKServer) healthy() bool {
	if s.health.Disabled {
		return true
	}

	s.healthMutex.RLock()
	defer s.healthMutex.RUnlock()
	return s.healthFailureCount < s.health.FailureThreshold
}

// NewSDKServerContext returns a Context that cancels when SIGTERM or os.Interrupt
// is received and the GameServer's Status is shutdown
func (s *SDKServer) NewSDKServerContext(ctx context.Context) context.Context {
	sdkCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-ctx.Done()
		s.logger.Info("SDK server shutdown requested, shutting down sdk server")
		cancel()
	}()
	return sdkCtx
}

func (s *SDKServer) flushGameServer(ctx context.Context, m *mutator.Mutator[agonesv1.GameServer], actions []mutator.Action[agonesv1.GameServer]) {
	var appliedActions []mutator.Action[agonesv1.GameServer]
	var failedActions []mutator.Action[agonesv1.GameServer]

	gs, err := s.gameServer()
	if err != nil {
		s.logger.WithError(err).Info("flushGameServer: error getting GameServer")
		m.Push(actions...)
		return
	}
	gsCopy := gs.DeepCopy()

	for _, action := range actions {
		err := action.Mutate(ctx, gsCopy)
		if err != nil {
			s.logger.WithError(err).WithField("action", action).Info("flushGameServer: error mutating GameServer")
			if errors.Is(err, mutator.RetryErr) {
				failedActions = append(failedActions, action)
				continue
			}
			if ch := action.ErrCh(); ch != nil {
				select {
				case ch <- err:
				default:
				}
			}
			continue
		}
		appliedActions = append(appliedActions, action)
	}

	gs, err = s.patchGameServer(ctx, gs, gsCopy)
	if err != nil {
		s.logger.WithError(err).Info("flushGameServer: error patching GameServer")
		m.Push(actions...)
		return
	}

	for _, act := range appliedActions {
		if err := act.Finalize(ctx, gsCopy); err != nil {
			s.logger.WithError(err).WithField("action", act).Info("flushGameServer: error finalizing GameServer")
		}
	}

	for _, f := range failedActions {
		m.Push(f)
	}
}

func waitForErrorOrClose(errCh <-chan error) error {
	for {
		select {
		case err, ok := <-errCh:
			if !ok {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
}
