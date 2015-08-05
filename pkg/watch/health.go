package watch

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/square/p2/pkg/health"
	"github.com/square/p2/pkg/kp"
	"github.com/square/p2/pkg/logging"
	"github.com/square/p2/pkg/pods"
	"github.com/square/p2/pkg/preparer"
)

// These constants should probably all be something the p2 user can set
// in their preparer config...

// Duration between reality store checks
const POLL_KV_FOR_PODS = 3 * time.Second

// Duration between health checks
const HEALTHCHECK_INTERVAL = 1 * time.Second

// Healthcheck TTL
const TTL = 60 * time.Second

// Contains method for watching the consul reality store to
// track services running on a node. A manager method:
// MonitorPodHealth tracks the reality store and manages
// a health checking go routine for each service in the
// reality store

// PodWatch houses a pod's manifest, a channel to kill the
// pod's goroutine if the pod is removed from the reality
// tree, and a bool that indicates whether or not the pod
// has a running MonitorHealth go routine
type PodWatch struct {
	manifest pods.Manifest

	// For tracking/controlling the go routine that performs health checks
	// on the pod associated with this PodWatch
	shutdownCh chan bool

	logger *logging.Logger
}

type StatusCheck struct {
	ID     string
	Node   string
	URI    string
	HTTP   bool
	Client *http.Client

	// the fields are provided so it can be determined if new health checks
	// actually need to be sent to consul. If newT - oldT << TTL and status
	// has not changed there is no reason to update consul
	lastCheck  time.Time          // time of last health check
	lastStatus health.HealthState // status of last health check

}

// MonitorPodHealth is meant to be a long running go routine.
// MonitorPodHealth reads from a consul store to determine which
// services should be running on the host. MonitorPodHealth
// runs a CheckHealth routine to monitor the health of each
// service and kills routines for services that should no
// longer be running.
func MonitorPodHealth(config *preparer.PreparerConfig, logger *logging.Logger, shutdownCh chan struct{}) {
	store, err := config.GetStore()
	if err != nil {
		// A bad config should have already produced a nice, user-friendly error message.
		logger.
			WithField("inner_err", err).
			Fatalf("error creating health monitor KV store")
	}
	client, err := config.GetClient()
	if err != nil {
		logger.
			WithField("inner_err", err).
			Fatalf("failed to get http client for this preparer")

	}

	node := config.NodeName
	pods := []PodWatch{}
	pods = updateHealthMonitors(store, client, pods, node, logger)
	for {
		select {
		case <-time.After(POLL_KV_FOR_PODS):
			// check if pods have been added or removed
			// starts monitor routine for new pods
			// kills monitor routine for removed pods
			pods = updateHealthMonitors(store, client, pods, node, logger)
		case <-shutdownCh:
			return
		}
	}
}

// Monitor Health is a go routine that runs as long as the
// service it is monitoring. Every HEALTHCHECK_INTERVAL it
// performs a health check and writes that information to
// consul
func (p *PodWatch) MonitorHealth(store kp.Store, statusChecker StatusCheck, shutdownCh chan bool) {
	for {
		select {
		case <-time.After(HEALTHCHECK_INTERVAL):
			p.checkHealth(store, statusChecker)
		case <-shutdownCh:
			return
		}
	}
}

func (p *PodWatch) checkHealth(store kp.Store, sc StatusCheck) {
	resp, err := sc.Check()
	health, err := resultFromCheck(resp, err)
	if err != nil {
		return
	}
	health.ID = sc.ID
	health.Node = sc.Node
	health.Service = sc.ID

	if sc.updateNeeded(health, TTL) {
		sc.lastCheck, err = writeToConsul(health, store)
		sc.lastStatus = health.Status
		if err != nil {
			p.logger.WithField("inner_err", err).Warningln("failed to write to consul")
		}
	}
}

func updateHealthMonitors(store kp.Store,
	client *http.Client,
	watchedPods []PodWatch,
	node string,
	logger *logging.Logger) []PodWatch {
	path := kp.RealityPath(node)
	reality, _, err := store.ListPods(path)
	if err != nil {
		logger.WithField("inner_err", err).Warningln("failed to get pods from reality store")
	}

	return updatePods(store, client, watchedPods, reality, node, logger)
}

func resultFromCheck(resp *http.Response, err error) (health.Result, error) {
	res := health.Result{}
	if err != nil || resp == nil {
		res.Status = health.Critical
		if err != nil {
			res.Output = err.Error()
		}
		return res, nil
	}

	res.Output, err = getBody(resp)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		res.Status = health.Passing
	} else {
		res.Status = health.Critical
	}
	return res, err
}

func getBody(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// once we get health data we need to make a put request
// to consul to put the data in the KV Store
func writeToConsul(res health.Result, store kp.Store) (time.Time, error) {
	timeOfPut, _, err := store.PutHealth(resToKPRes(res))
	return timeOfPut, err
}

func resToKPRes(res health.Result) kp.WatchResult {
	return kp.WatchResult{
		Service: res.Service,
		Node:    res.Node,
		Id:      res.ID,
		Status:  string(res.Status),
		Output:  res.Output,
	}
}

// compares services being monitored with services that
// need to be monitored.
func updatePods(store kp.Store,
	client *http.Client,
	current []PodWatch,
	reality []kp.ManifestResult,
	node string,
	logger *logging.Logger) []PodWatch {
	newCurrent := []PodWatch{}
	// for pod in current if pod not in reality: kill
	for _, pod := range current {
		inReality := false
		for _, man := range reality {
			if man.Manifest.Id == pod.manifest.Id {
				inReality = true
				break
			}
		}

		// if this podwatch is not in the reality store kill its go routine
		// else add this podwatch to newCurrent
		if inReality == false {
			pod.shutdownCh <- true
		} else {
			newCurrent = append(newCurrent, pod)
		}
	}
	// for pod in reality if pod not in current: create podwatch and
	// append to current
	for _, man := range reality {
		missing := true
		for _, pod := range newCurrent {
			if man.Manifest.Id == pod.manifest.Id {
				missing = false
				break
			}
		}

		// if a manifest is in reality but not current a podwatch is created
		// with that manifest and added to newCurrent
		if missing && man.Manifest.StatusPort != 0 {
			newPod := PodWatch{
				manifest:   man.Manifest,
				shutdownCh: make(chan bool, 1),
				logger:     logger,
			}

			// Each health monitor will have its own statusChecker
			sc := StatusCheck{
				ID:     newPod.manifest.Id,
				Node:   node,
				URI:    fmt.Sprintf("%s:%d", node, newPod.manifest.StatusPort),
				Client: client,
				HTTP:   newPod.manifest.StatusHTTP,
			}
			go newPod.MonitorHealth(store, sc, newPod.shutdownCh)
			newCurrent = append(newCurrent, newPod)
		}
	}
	return newCurrent
}

func (sc *StatusCheck) updateNeeded(res health.Result, ttl time.Duration) bool {
	// if status has changed indicate that consul needs to be updated
	if sc.lastStatus != res.Status {
		return true
	}
	// if more than TTL / 4 seconds have elapsed since previous check
	// indicate that consul needs to be updated
	if time.Since(sc.lastCheck) > time.Duration(ttl/4)*time.Second {
		return true
	}

	return false
}

func (sc *StatusCheck) Check() (*http.Response, error) {
	if sc.HTTP {
		return kp.HttpsStatusCheck(sc.Client, sc.URI)
	}
	return kp.HttpStatusCheck(sc.Client, sc.URI)
}
