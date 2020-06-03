package health

import (
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/abh/geodns/applog"
	"github.com/abh/geodns/typeutil"

	"github.com/miekg/dns"
)

var (
	Qtypes = []uint16{dns.TypeA, dns.TypeAAAA}
)

type HealthTester interface {
	String() string
	Test(*HealthTest) bool
}

type HealthTestParameters struct {
	Frequency        time.Duration
	RetryTime        time.Duration
	timeout          time.Duration
	Retries          int
	HealthyInitially bool
	TestName         string
	global           bool
}

type HealthTest struct {
	HealthTestParameters
	IpAddress    net.IP
	Healthy      bool
	healthyMutex sync.RWMutex
	closing      chan chan error
	health       chan bool
	tester       *HealthTester
	globalMap    map[string]bool
}

type HealthTestRunnerEntry struct {
	HealthTest
	references map[string]bool
}

type HealthTestRunner struct {
	entries    map[string]*HealthTestRunnerEntry
	entryMutex sync.RWMutex
}

var TestRunner = &HealthTestRunner{
	entries: make(map[string]*HealthTestRunnerEntry),
}

func DefaultHealthTestParameters() HealthTestParameters {
	return HealthTestParameters{
		Frequency:        30 * time.Second,
		RetryTime:        5 * time.Second,
		timeout:          5 * time.Second,
		Retries:          3,
		HealthyInitially: false,
	}
}

func NewTest(ipAddress net.IP, htp HealthTestParameters, tester *HealthTester) *HealthTest {
	ht := HealthTest{
		IpAddress:            ipAddress,
		HealthTestParameters: htp,
		Healthy:              true,
		tester:               tester,
		globalMap:            make(map[string]bool),
	}
	ht.Healthy = ht.HealthyInitially
	if ht.Frequency < time.Second {
		ht.Frequency = time.Second
	}
	if ht.RetryTime < time.Second {
		ht.RetryTime = time.Second
	}
	if ht.timeout < time.Second {
		ht.timeout = time.Second
	}
	return &ht
}

// Format the health test as a string - used to compare two tests and as an index for the hash
func (ht *HealthTest) String() string {
	ip := ht.IpAddress.String()
	if ht.HealthTestParameters.global {
		ip = "" // ensure we have a single instance of a global health check with the same paramaters
	}
	return fmt.Sprintf("%s/%v/%s", ip, ht.HealthTestParameters, (*ht.tester).String())
}

// safe copy function that copies the parameters but not (e.g.) the
// mutex
func (ht *HealthTest) Copy(ipAddress net.IP) *HealthTest {
	return NewTest(ipAddress, ht.HealthTestParameters, ht.tester)
}

func (ht *HealthTest) setGlobal(g map[string]bool) {
	ht.healthyMutex.Lock()
	defer ht.healthyMutex.Unlock()
	ht.globalMap = g
}

func (ht *HealthTest) getGlobal(k string) (bool, bool) {
	ht.healthyMutex.RLock()
	defer ht.healthyMutex.RUnlock()
	healthy, ok := ht.globalMap[k]
	return healthy, ok
}

func (ht *HealthTest) run() {
	randomDelay := rand.Int63n(ht.Frequency.Nanoseconds())
	if !ht.isHealthy() {
		randomDelay = rand.Int63n(ht.RetryTime.Nanoseconds())
	}
	var nextPoll time.Time = time.Now().Add(time.Duration(randomDelay))
	var pollStart time.Time
	failCount := 0
	for {
		var pollDelay time.Duration
		if now := time.Now(); nextPoll.After(now) {
			pollDelay = nextPoll.Sub(now)
		}
		var startPoll <-chan time.Time
		var closingPoll <-chan chan error
		if pollStart.IsZero() {
			closingPoll = ht.closing
			startPoll = time.After(pollDelay)
		}
		select {
		case errc := <-closingPoll: // don't close while we are polling or we send to a closed channel
			errc <- nil
			return
		case <-startPoll:
			pollStart = time.Now()
			go ht.poll()
		case h := <-ht.health:
			nextPoll = pollStart.Add(ht.Frequency)
			if h {
				ht.setHealthy(true)
				failCount = 0
			} else {
				failCount++
				applog.Printf("Failure for %s, retry count=%d, healthy=%v", ht.IpAddress, failCount, ht.isHealthy())
				if failCount >= ht.Retries {
					ht.setHealthy(false)
					nextPoll = pollStart.Add(ht.RetryTime)
				}
			}
			pollStart = time.Time{}
			applog.Printf("Check result for %s health=%v, next poll at %s", ht.IpAddress, h, nextPoll)
			//randomDelay := rand.Int63n(time.Second.Nanoseconds())
			//nextPoll = nextPoll.Add(time.Duration(randomDelay))
		}
	}
}

func (ht *HealthTest) poll() {
	applog.Printf("Checking health of %s", ht.IpAddress)
	result := (*ht.tester).Test(ht)
	applog.Printf("Checked health of %s, healthy=%v", ht.IpAddress, result)
	ht.health <- result
}

func (ht *HealthTest) start() {
	ht.closing = make(chan chan error)
	ht.health = make(chan bool)
	applog.Printf("Starting health test on %s, frequency=%s, retry_time=%s, timeout=%s, retries=%d", ht.IpAddress, ht.Frequency, ht.RetryTime, ht.timeout, ht.Retries)
	go ht.run()
}

// Stop the health check from running
func (ht *HealthTest) Stop() (err error) {
	// Check it's been started by existing of the closing channel
	if ht.closing == nil {
		return nil
	}
	applog.Printf("Stopping health test on %s", ht.IpAddress)
	errc := make(chan error)
	ht.closing <- errc
	err = <-errc
	close(ht.closing)
	ht.closing = nil
	close(ht.health)
	ht.health = nil
	return err
}

func (ht *HealthTest) IP() net.IP {
	return ht.IpAddress
}
func (ht *HealthTest) IsHealthy() bool {
	return ht.isHealthy()
}

func (ht *HealthTest) isHealthy() bool {
	ht.healthyMutex.RLock()
	h := ht.Healthy
	ht.healthyMutex.RUnlock()
	return h
}

func (ht *HealthTest) setHealthy(h bool) {
	ht.healthyMutex.Lock()
	old := ht.Healthy
	ht.Healthy = h
	ht.healthyMutex.Unlock()
	if old != h {
		applog.Printf("Changing health status of %s from %v to %v", ht.IpAddress, old, h)
	}
}

func (htr *HealthTestRunner) AddTest(ht *HealthTest, ref string) {
	key := ht.String()
	htr.entryMutex.Lock()
	defer htr.entryMutex.Unlock()
	if t, ok := htr.entries[key]; ok {
		// we already have an instance of this test running. Record we are using it
		t.references[ref] = true
	} else {
		// a test that isn't running. Record we are using it and start the test
		t := &HealthTestRunnerEntry{
			HealthTest: *ht.Copy(ht.IpAddress),
			references: make(map[string]bool),
		}
		if t.global {
			t.IpAddress = nil
		}
		// we know it is not started, so no need for the mutex
		t.Healthy = ht.Healthy
		t.references[ref] = true
		t.start()
		htr.entries[key] = t
	}
}

func (htr *HealthTestRunner) RemoveTest(ht *HealthTest, ref string) {
	key := ht.String()
	htr.entryMutex.Lock()
	defer htr.entryMutex.Unlock()
	if t, ok := htr.entries[key]; ok {
		delete(t.references, ref)
		// record the last state of health
		ht.healthyMutex.Lock()
		ht.Healthy = t.isHealthy()
		ht.healthyMutex.Unlock()
		if len(t.references) == 0 {
			// no more references, delete the test
			t.Stop()
			delete(htr.entries, key)
		}
	}
}

func (htr *HealthTestRunner) refAllGlobalHealthChecks(ref string, add bool) {
	htr.entryMutex.Lock()
	defer htr.entryMutex.Unlock()
	for key, t := range htr.entries {
		if t.global {
			if add {
				t.references[ref] = true
			} else {
				delete(t.references, ref)
				if len(t.references) == 0 {
					// no more references, delete the test
					t.Stop()
					delete(htr.entries, key)
				}
			}
		}
	}
}

func (htr *HealthTestRunner) IsHealthy(ht *HealthTest) bool {
	return htr.isHealthy(ht)
}

func (htr *HealthTestRunner) isHealthy(ht *HealthTest) bool {
	key := ht.String()
	htr.entryMutex.RLock()
	defer htr.entryMutex.RUnlock()
	if t, ok := htr.entries[key]; ok {
		if t.global {
			healthy, ok := t.getGlobal(ht.IpAddress.String())
			if ok {
				return healthy
			}
		} else {
			return t.isHealthy()
		}
	}
	return ht.isHealthy()
}

func NewFromMap(i map[string]interface{}) (*HealthTest, error) {
	ts := typeutil.ToString(i["type"])

	if len(ts) == 0 {
		return nil, fmt.Errorf("type required")
	}

	htp := DefaultHealthTestParameters()
	nh, ok := HealthTesterMap[ts]
	if !ok {
		return nil, fmt.Errorf("Bad health test type '%s'", ts)
	}

	htp.TestName = ts
	h := nh(i, &htp)

	for k, v := range i {
		switch k {
		case "frequency":
			htp.Frequency = time.Duration(typeutil.ToInt(v)) * time.Second
		case "retry_time":
			htp.RetryTime = time.Duration(typeutil.ToInt(v)) * time.Second
		case "timeout":
			htp.RetryTime = time.Duration(typeutil.ToInt(v)) * time.Second
		case "retries":
			htp.Retries = typeutil.ToInt(v)
		case "healthy_initially":
			htp.HealthyInitially = typeutil.ToBool(v)
			// applog.Printf("HealthyInitially for %s is %v", l.Label, htp.healthyInitially)
		}
	}

	tester := NewTest(nil, htp, &h)
	return tester, nil

}
