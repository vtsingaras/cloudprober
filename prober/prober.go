// Copyright 2017-2019 The Cloudprober Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*
Package prober provides a prober for running a set of probes.

Prober takes in a config proto which dictates what probes should be created
with what configuration, and manages the asynchronous fan-in/fan-out of the
metrics data from these probes.
*/
package prober

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"sync"
	"time"

	configpb "github.com/cloudprober/cloudprober/config/proto"
	"github.com/cloudprober/cloudprober/config/runconfig"
	"github.com/cloudprober/cloudprober/logger"
	"github.com/cloudprober/cloudprober/metrics"
	spb "github.com/cloudprober/cloudprober/prober/proto"
	"github.com/cloudprober/cloudprober/probes"
	"github.com/cloudprober/cloudprober/probes/options"
	probes_configpb "github.com/cloudprober/cloudprober/probes/proto"
	rdsserver "github.com/cloudprober/cloudprober/rds/server"
	"github.com/cloudprober/cloudprober/servers"
	"github.com/cloudprober/cloudprober/surfacers"
	"github.com/cloudprober/cloudprober/sysvars"
	"github.com/cloudprober/cloudprober/targets"
	"github.com/cloudprober/cloudprober/targets/endpoint"
	"github.com/cloudprober/cloudprober/targets/lameduck"
	"github.com/golang/glog"
	"github.com/google/go-cpy/cpy"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Prober represents a collection of probes where each probe implements the Probe interface.
type Prober struct {
	Probes    map[string]*probes.ProbeInfo
	Servers   []*servers.ServerInfo
	c         *configpb.ProberConfig
	l         *logger.Logger
	mu        sync.Mutex
	ldLister  endpoint.Lister
	Surfacers []*surfacers.SurfacerInfo

	// Context to use when starting probes
	probeStartContext context.Context

	// Per-probe cancelFunc map.
	probeCancelFunc map[string]context.CancelFunc

	// dataChan for passing metrics between probes and main goroutine.
	dataChan chan *metrics.EventMetrics

	// Used by GetConfig for /config handler.
	TextConfig string

	// Required for all gRPC server implementations.
	spb.UnimplementedCloudproberServer
}

func runOnThisHost(runOn string, hostname string) (bool, error) {
	if runOn == "" {
		return true, nil
	}
	r, err := regexp.Compile(runOn)
	if err != nil {
		return false, err
	}
	return r.MatchString(hostname), nil
}

func (pr *Prober) addProbe(p *probes_configpb.ProbeDef) error {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	// Check if this probe is supposed to run here.
	runHere, err := runOnThisHost(p.GetRunOn(), sysvars.Vars()["hostname"])
	if err != nil {
		return err
	}
	if !runHere {
		return nil
	}

	if pr.Probes[p.GetName()] != nil {
		return status.Errorf(codes.AlreadyExists, "probe %s is already defined", p.GetName())
	}

	opts, err := options.BuildProbeOptions(p, pr.ldLister, pr.c.GetGlobalTargetsOptions(), pr.l)
	if err != nil {
		return status.Errorf(codes.Unknown, err.Error())
	}

	pr.l.Infof("Creating a %s probe: %s", p.GetType(), p.GetName())
	probeInfo, err := probes.CreateProbe(p, opts)
	if err != nil {
		return status.Errorf(codes.Unknown, err.Error())
	}
	pr.Probes[p.GetName()] = probeInfo

	return nil
}

func (pr *Prober) removeProbe(name string) error {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	if pr.Probes[name] == nil {
		return status.Errorf(codes.NotFound, "removeProbe called for non-existent probe: %s", name)
	}
	pr._stopProbeWithNoLock(name)
	delete(pr.Probes, name)
	return nil
}

// Init initialize prober with the given config file.
func (pr *Prober) Init(ctx context.Context, cfg *configpb.ProberConfig, l *logger.Logger) error {
	pr.c = cfg
	pr.l = l

	// Initialize cloudprober gRPC service if configured.
	srv := runconfig.DefaultGRPCServer()
	if srv != nil {
		spb.RegisterCloudproberServer(srv, pr)
	}

	// Initialize RDS server, if configured and attach to the default gRPC server.
	// Note that we can still attach services to the default gRPC server as it's
	// started later in Start().
	if c := pr.c.GetRdsServer(); c != nil {
		l, err := logger.NewCloudproberLog("rds-server")
		if err != nil {
			return err
		}
		rdsServer, err := rdsserver.New(ctx, c, nil, l)
		if err != nil {
			return err
		}

		runconfig.SetLocalRDSServer(rdsServer)
		if srv != nil {
			rdsServer.RegisterWithGRPC(srv)
		}
	}

	// Initialize lameduck lister
	globalTargetsOpts := pr.c.GetGlobalTargetsOptions()

	if globalTargetsOpts.GetLameDuckOptions() != nil {
		ldLogger, err := logger.NewCloudproberLog("lame-duck")
		if err != nil {
			return fmt.Errorf("error in initializing lame-duck logger: %v", err)
		}

		if err := lameduck.InitDefaultLister(globalTargetsOpts, nil, ldLogger); err != nil {
			return err
		}

		pr.ldLister, err = lameduck.GetDefaultLister()
		if err != nil {
			pr.l.Warningf("Error while getting default lameduck lister, lameduck behavior will be disabled. Err: %v", err)
		}
	}
	var err error

	// Initialize shared targets
	for _, st := range pr.c.GetSharedTargets() {
		tgts, err := targets.New(st.GetTargets(), pr.ldLister, globalTargetsOpts, pr.l, pr.l)
		if err != nil {
			return err
		}
		targets.SetSharedTargets(st.GetName(), tgts)
	}

	// Initiliaze probes
	pr.Probes = make(map[string]*probes.ProbeInfo)
	pr.probeCancelFunc = make(map[string]context.CancelFunc)
	for _, p := range pr.c.GetProbe() {
		if err := pr.addProbe(p); err != nil {
			return err
		}
	}

	// Initialize servers
	pr.Servers, err = servers.Init(ctx, pr.c.GetServer())
	if err != nil {
		return err
	}

	pr.Surfacers, err = surfacers.Init(ctx, pr.c.GetSurfacer())
	if err != nil {
		return err
	}

	return nil
}

// Start starts a previously initialized Cloudprober.
func (pr *Prober) Start(ctx context.Context) {
	pr.probeStartContext = ctx
	pr.dataChan = make(chan *metrics.EventMetrics, 100000)

	go func() {
		var em *metrics.EventMetrics
		for {
			em = <-pr.dataChan
			var s = em.String()
			if len(s) > logger.MaxLogEntrySize {
				glog.Warningf("Metric entry for timestamp %v dropped due to large size: %d", em.Timestamp, len(s))
				continue
			}

			// Replicate the surfacer message to every surfacer we have
			// registered. Note that s.Write() is expected to be
			// non-blocking to avoid blocking of EventMetrics message
			// processing.
			for _, surfacer := range pr.Surfacers {
				surfacer.Write(context.Background(), em)
			}
		}
	}()

	// Start a goroutine to export system variables
	go sysvars.Start(ctx, pr.dataChan, time.Millisecond*time.Duration(pr.c.GetSysvarsIntervalMsec()), pr.c.GetSysvarsEnvVar())

	// Start servers, each in its own goroutine
	for _, s := range pr.Servers {
		go s.Start(ctx, pr.dataChan)
	}

	if pr.c.GetDisableJitter() {
		for name := range pr.Probes {
			go pr.startProbe(ctx, name)
		}
	} else {
		pr.startProbesWithJitter(ctx)
	}
}

// Starts a probe without acquiring the lock
func (pr *Prober) _startProbeWithNoLock(ctx context.Context, name string) {
	probeCtx, cancelFunc := context.WithCancel(ctx)
	pr.probeCancelFunc[name] = cancelFunc
	go pr.Probes[name].Start(probeCtx, pr.dataChan)
}
func (pr *Prober) startProbe(ctx context.Context, name string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	pr._startProbeWithNoLock(ctx, name)
}

// StartProbe starts a single probe using the initial Prober context
func (pr *Prober) StartProbe(name string) {
	pr.startProbe(pr.probeStartContext, name)
}

// StartAllProbes starts all registered probes using the initial Prober context
func (pr *Prober) StartAllProbes() {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	for name := range pr.Probes {
		pr._startProbeWithNoLock(pr.probeStartContext, name)
	}
}

// Stops a probe without acquiring the lock
func (pr *Prober) _stopProbeWithNoLock(name string) {
	if pr.Probes[name] == nil {
		pr.l.Criticalf("stopProbe called on non-existent probe: %s", name)
	}
	if pr.probeCancelFunc[name] == nil {
		pr.l.Infof("stopProbe called on not-started probe: %s", name)
	} else {
		pr.probeCancelFunc[name]()
		delete(pr.probeCancelFunc, name)
	}
}

// StopProbe stops a probe by name
func (pr *Prober) StopProbe(name string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	pr._stopProbeWithNoLock(name)
}

// StopAllProbes stops all currently running probes
func (pr *Prober) StopAllProbes() {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	for name := range pr.Probes {
		pr._stopProbeWithNoLock(name)
	}
}

func (pr *Prober) getProbes() map[string]*probes.ProbeInfo {
	pr.mu.Lock()
	defer pr.mu.Unlock()

	var copier = cpy.New(
		cpy.Func(proto.Clone),
		cpy.IgnoreAllUnexported(),
	)
	result := copier.Copy(pr.Probes).(map[string]*probes.ProbeInfo)
	return result
}

// startProbesWithJitter try to space out probes over time, as much as possible,
// without making it too complicated. We arrange probes into interval buckets -
// all probes with the same interval will be part of the same bucket, and we
// then spread out probes within that interval by introducing a delay of
// interval / len(probes) between probes. We also introduce a random jitter
// between different interval buckets.
func (pr *Prober) startProbesWithJitter(ctx context.Context) {
	// Seed random number generator.
	rand.Seed(time.Now().UnixNano())

	// Make interval -> [probe1, probe2, probe3..] map
	intervalBuckets := make(map[time.Duration][]*probes.ProbeInfo)
	for _, p := range pr.Probes {
		intervalBuckets[p.Options.Interval] = append(intervalBuckets[p.Options.Interval], p)
	}

	for interval, probeInfos := range intervalBuckets {
		go func(interval time.Duration, probeInfos []*probes.ProbeInfo) {
			// Introduce a random jitter between interval buckets.
			randomDelayMsec := rand.Int63n(int64(interval.Seconds() * 1000))
			time.Sleep(time.Duration(randomDelayMsec) * time.Millisecond)

			interProbeDelay := interval / time.Duration(len(probeInfos))

			// Spread out probes evenly with an interval bucket.
			for _, p := range probeInfos {
				pr.l.Info("Starting probe: ", p.Name)
				go pr.startProbe(ctx, p.Name)
				time.Sleep(interProbeDelay)
			}
		}(interval, probeInfos)
	}
}
