package veneur

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"sync"

	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stripe/veneur/protocol"
	"github.com/stripe/veneur/samplers"
	"github.com/stripe/veneur/sinks"
	"github.com/stripe/veneur/sinks/blackhole"
	"github.com/stripe/veneur/ssf"
	"github.com/stripe/veneur/tdigest"
	"github.com/stripe/veneur/trace"
)

const ε = .00002

const DefaultFlushInterval = 50 * time.Millisecond
const DefaultServerTimeout = 100 * time.Millisecond

var DebugMode bool

func seedRand() {
	seed := time.Now().Unix()
	log.WithFields(logrus.Fields{
		"randSeed": seed,
	}).Info("Re-seeding random number generator")
	rand.Seed(seed)
}

func TestMain(m *testing.M) {
	flag.Parse()
	DebugMode = flag.Lookup("test.v").Value.(flag.Getter).Get().(bool)
	os.Exit(m.Run())
}

// On the CI server, we can't be guaranteed that the port will be
// released immediately after the server is shut down. Instead, use
// a unique port for each test. As long as we don't have an insane number
// of integration tests, we should be fine.
var HTTPAddrPort = 8129

// set up a boilerplate local config for later use
func localConfig() Config {
	return generateConfig("http://localhost")
}

// set up a boilerplate global config for later use
func globalConfig() Config {
	return generateConfig("")
}

// generateConfig is not called config to avoid
// accidental variable shadowing
func generateConfig(forwardAddr string) Config {
	// we don't shut down ports so avoid address in use errors
	port := HTTPAddrPort
	HTTPAddrPort++
	metricsPort := HTTPAddrPort
	HTTPAddrPort++
	tracePort := HTTPAddrPort
	HTTPAddrPort++

	return Config{
		Debug:    DebugMode,
		Hostname: "localhost",

		// Use a shorter interval for tests
		Interval:              DefaultFlushInterval.String(),
		Key:                   "",
		MetricMaxLength:       4096,
		Percentiles:           []float64{.5, .75, .99},
		Aggregates:            []string{"min", "max", "count"},
		ReadBufferSizeBytes:   2097152,
		StatsdListenAddresses: []string{fmt.Sprintf("udp://localhost:%d", metricsPort)},
		HTTPAddress:           fmt.Sprintf("localhost:%d", port),
		ForwardAddress:        forwardAddr,
		NumWorkers:            4,
		FlushFile:             "",

		// Use only one reader, so that we can run tests
		// on platforms which do not support SO_REUSEPORT
		NumReaders: 1,

		// Currently this points nowhere, which is intentional.
		// We don't need internal metrics for the tests, and they make testing
		// more complicated.
		StatsAddress:    "localhost:8125",
		Tags:            []string{},
		SentryDsn:       "",
		FlushMaxPerBody: 1024,

		// Don't use the default port 8128: Veneur sends its own traces there, causing failures
		SsfListenAddresses:  []string{fmt.Sprintf("udp://127.0.0.1:%d", tracePort)},
		TraceMaxLengthBytes: 4096,
		SsfBufferSize:       32,
	}
}

func generateMetrics() (metricValues []float64, expectedMetrics map[string]float64) {
	metricValues = []float64{1.0, 2.0, 7.0, 8.0, 100.0}

	expectedMetrics = map[string]float64{
		"a.b.c.max": 100,
		"a.b.c.min": 1,

		// Count is normalized by second
		// so 5 values/50ms = 100 values/s
		"a.b.c.count": float64(len(metricValues)) * float64(time.Second) / float64(DefaultFlushInterval),

		// tdigest approximation causes this to be off by 1
		"a.b.c.50percentile": 6,
		"a.b.c.75percentile": 42,
		"a.b.c.99percentile": 98,
	}
	return metricValues, expectedMetrics
}

// setupVeneurServer creates a local server from the specified config
// and starts listening for requests. It returns the server for inspection.
// If no metricSink or spanSink are provided then a `black hole` sink be used
// so that flushes to these sinks do "nothing".
func setupVeneurServer(t testing.TB, config Config, transport http.RoundTripper, mSink sinks.MetricSink, sSink sinks.SpanSink) *Server {
	server, err := NewFromConfig(config)
	if transport != nil {
		server.HTTPClient.Transport = transport
	}
	if err != nil {
		t.Fatal(err)
	}

	if transport != nil {
		server.HTTPClient.Transport = transport
	}
	server.TraceClient = nil

	if mSink == nil {
		// Install a blackhole sink if we have no other sinks
		bhs, _ := blackhole.NewBlackholeMetricSink()
		mSink = bhs
	}
	server.metricSinks = append(server.metricSinks, mSink)

	if sSink == nil {
		// Install a blackhole sink if we have no other sinks
		bhs, _ := blackhole.NewBlackholeSpanSink()
		sSink = bhs
	}
	server.spanSinks = append(server.spanSinks, sSink)

	server.Start()

	go server.HTTPServe()
	return server
}

type channelMetricSink struct {
	metricsChannel chan []samplers.InterMetric
}

// NewChannelMetricSink creates a new channelMetricSink. This sink writes any
// flushed metrics to its `metricsChannel` such that the test can inspect
// the metrics for correctness.
func NewChannelMetricSink(ch chan []samplers.InterMetric) (*channelMetricSink, error) {
	return &channelMetricSink{
		metricsChannel: ch,
	}, nil
}

func (c *channelMetricSink) Name() string {
	return "channel"
}

func (c *channelMetricSink) Start(*trace.Client) error {
	return nil
}

func (c *channelMetricSink) Flush(ctx context.Context, metrics []samplers.InterMetric) error {
	// Put the whole slice in since many tests want to see all of them and we
	// don't want them to have to loop over and wait on empty or something
	c.metricsChannel <- metrics
	return nil
}

func (c *channelMetricSink) FlushEventsChecks(ctx context.Context, events []samplers.UDPEvent, checks []samplers.UDPServiceCheck) {
	return
}

// fixture sets up a mock Datadog API server and Veneur
type fixture struct {
	api             *httptest.Server
	server          *Server
	interval        time.Duration
	flushMaxPerBody int
}

func newFixture(t testing.TB, config Config, mSink sinks.MetricSink, sSink sinks.SpanSink) *fixture {
	interval, err := config.ParseInterval()
	assert.NoError(t, err)

	// Set up a remote server (the API that we're sending the data to)
	// (e.g. Datadog)
	f := &fixture{nil, &Server{}, interval, config.FlushMaxPerBody}

	config.NumWorkers = 1
	f.server = setupVeneurServer(t, config, nil, mSink, sSink)
	return f
}

func (f *fixture) Close() {
	// Make it safe to close this more than once, jic
	if f.server != nil {
		f.server.Shutdown()
		f.server = nil
	}
}

// TestLocalServerUnaggregatedMetrics tests the behavior of
// the veneur client when operating without a global veneur
// instance (ie, when sending data directly to the remote server)
func TestLocalServerUnaggregatedMetrics(t *testing.T) {
	metricValues, _ := generateMetrics()
	config := localConfig()
	config.Tags = []string{"butts:farts"}

	metricsChan := make(chan []samplers.InterMetric, 10)
	cms, _ := NewChannelMetricSink(metricsChan)
	defer close(metricsChan)

	f := newFixture(t, config, cms, nil)
	defer f.Close()

	for _, value := range metricValues {
		f.server.Workers[0].ProcessMetric(&samplers.UDPMetric{
			MetricKey: samplers.MetricKey{
				Name: "a.b.c",
				Type: "histogram",
			},
			Value:      value,
			Digest:     12345,
			SampleRate: 1.0,
			Scope:      samplers.LocalOnly,
		})
	}

	f.server.Flush(context.TODO())

	interMetrics := <-metricsChan
	assert.Equal(t, 6, len(interMetrics), "incorrect number of elements in the flushed series on the remote server")
}

func TestGlobalServerFlush(t *testing.T) {
	metricValues, expectedMetrics := generateMetrics()
	config := globalConfig()

	metricsChan := make(chan []samplers.InterMetric, 10)
	cms, _ := NewChannelMetricSink(metricsChan)
	defer close(metricsChan)

	f := newFixture(t, config, cms, nil)
	defer f.Close()

	for _, value := range metricValues {
		f.server.Workers[0].ProcessMetric(&samplers.UDPMetric{
			MetricKey: samplers.MetricKey{
				Name: "a.b.c",
				Type: "histogram",
			},
			Value:      value,
			Digest:     12345,
			SampleRate: 1.0,
			Scope:      samplers.LocalOnly,
		})
	}

	f.server.Flush(context.TODO())

	interMetrics := <-metricsChan
	assert.Equal(t, len(expectedMetrics), len(interMetrics), "incorrect number of elements in the flushed series on the remote server")
}

func TestLocalServerMixedMetrics(t *testing.T) {
	// The exact gob stream that we will receive might differ, so we can't
	// test against the bytestream directly. But the two streams should unmarshal
	// to t-digests that have the same key properties, so we can test
	// those.
	const ExpectedGobStream = "\r\xff\x87\x02\x01\x02\xff\x88\x00\x01\xff\x84\x00\x007\xff\x83\x03\x01\x01\bCentroid\x01\xff\x84\x00\x01\x03\x01\x04Mean\x01\b\x00\x01\x06Weight\x01\b\x00\x01\aSamples\x01\xff\x86\x00\x00\x00\x17\xff\x85\x02\x01\x01\t[]float64\x01\xff\x86\x00\x01\b\x00\x00/\xff\x88\x00\x05\x01\xfe\xf0?\x01\xfe\xf0?\x00\x01@\x01\xfe\xf0?\x00\x01\xfe\x1c@\x01\xfe\xf0?\x00\x01\xfe @\x01\xfe\xf0?\x00\x01\xfeY@\x01\xfe\xf0?\x00\x05\b\x00\xfeY@\x05\b\x00\xfe\xf0?\x05\b\x00\xfeY@"
	tdExpected := tdigest.NewMerging(100, false)
	err := tdExpected.GobDecode([]byte(ExpectedGobStream))
	assert.NoError(t, err, "Should not have encountered error in decoding expected gob stream")

	var HistogramValues = []float64{1.0, 2.0, 7.0, 8.0, 100.0}

	// Number of events observed (in 50ms interval)
	var HistogramCountRaw = len(HistogramValues)

	// Normalize to events/second
	// Explicitly convert to int to avoid confusing Stringer behavior
	var HistogramCountNormalized = float64(HistogramCountRaw) * float64(time.Second) / float64(DefaultFlushInterval)

	// Number of events observed
	const CounterNumEvents = 40

	expectedMetrics := map[string]float64{
		// 40 events/50ms = 800 events/s
		"x.y.z":     CounterNumEvents * float64(time.Second) / float64(DefaultFlushInterval),
		"a.b.c.max": 100,
		"a.b.c.min": 1,

		// Count is normalized by second
		// so 5 values/50ms = 100 values/s
		"a.b.c.count": float64(HistogramCountNormalized),
	}

	// This represents the global veneur instance, which receives request from
	// the local veneur instances, aggregates the data, and sends it to the remote API
	// (e.g. Datadog)
	globalTD := make(chan *tdigest.MergingDigest)
	globalVeneur := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Path, "/import", "Global veneur should receive request on /import path")

		zr, err := zlib.NewReader(r.Body)
		if err != nil {
			t.Fatal(err)
		}

		type requestItem struct {
			Name      string      `json:"name"`
			Tags      interface{} `json:"tags"`
			Tagstring string      `json:"tagstring"`
			Type      string      `json:"type"`
			Value     []byte      `json:"value"`
		}

		var metrics []requestItem

		err = json.NewDecoder(zr).Decode(&metrics)
		if err != nil {
			t.Fatal(err)
		}

		assert.Equal(t, 1, len(metrics), "incorrect number of elements in the flushed series")

		td := tdigest.NewMerging(100, false)
		err = td.GobDecode(metrics[0].Value)
		assert.NoError(t, err, "Should not have encountered error in decoding gob stream")
		globalTD <- td
		w.WriteHeader(http.StatusAccepted)
	}))
	defer globalVeneur.Close()

	config := localConfig()
	config.ForwardAddress = globalVeneur.URL
	f := newFixture(t, config, nil, nil)
	defer f.Close()

	// Create non-local metrics that should be passed to the global veneur instance
	for _, value := range HistogramValues {
		f.server.Workers[0].ProcessMetric(&samplers.UDPMetric{
			MetricKey: samplers.MetricKey{
				Name: "a.b.c",
				Type: "histogram",
			},
			Value:      value,
			Digest:     12345,
			SampleRate: 1.0,
			Scope:      samplers.MixedScope,
		})
	}

	// Create local-only metrics that should be passed directly to the remote API
	for i := 0; i < CounterNumEvents; i++ {
		f.server.Workers[0].ProcessMetric(&samplers.UDPMetric{
			MetricKey: samplers.MetricKey{
				Name: "x.y.z",
				Type: "counter",
			},
			Value:      1.0,
			Digest:     12345,
			SampleRate: 1.0,
			Scope:      samplers.LocalOnly,
		})
	}

	f.server.Flush(context.TODO())

	// the global veneur instance should get valid data
	td := <-globalTD
	assert.Equal(t, expectedMetrics["a.b.c.min"], td.Min(), "Minimum value is incorrect")
	assert.Equal(t, expectedMetrics["a.b.c.max"], td.Max(), "Maximum value is incorrect")

	// The remote server receives the raw count, *not* the normalized count
	assert.InEpsilon(t, HistogramCountRaw, td.Count(), ε)
	assert.Equal(t, tdExpected, td, "Underlying tdigest structure is incorrect")
}

func TestSplitBytes(t *testing.T) {
	seedRand()
	buf := make([]byte, 1000)

	for i := 0; i < 1000; i++ {
		// we construct a string of random length which is approximately 1/3rd A
		// and the other 2/3rds B
		buf = buf[:rand.Intn(cap(buf))]
		for i := range buf {
			if rand.Intn(3) == 0 {
				buf[i] = 'A'
			} else {
				buf[i] = 'B'
			}
		}
		checkBufferSplit(t, buf)
		buf = buf[:cap(buf)]
	}

	// also test pathological cases that the fuzz is unlikely to find
	checkBufferSplit(t, nil)
	checkBufferSplit(t, []byte{})
}

func checkBufferSplit(t *testing.T, buf []byte) {
	var testSplit [][]byte
	sb := samplers.NewSplitBytes(buf, 'A')
	for sb.Next() {
		testSplit = append(testSplit, sb.Chunk())
	}

	// now compare our split to the "real" implementation of split
	assert.EqualValues(t, bytes.Split(buf, []byte{'A'}), testSplit, "should have split %s correctly", buf)
}

func readTestKeysCerts() (map[string]string, error) {
	// reads the insecure test keys and certificates in fixtures generated with:
	// # Generate the authority key and certificate (512-bit RSA signed using SHA-256)
	// openssl genrsa -out cakey.pem 512
	// openssl req -new -x509 -key cakey.pem -out cacert.pem -days 1095 -subj "/O=Example Inc/CN=Example Certificate Authority"

	// # Generate the server key and certificate, signed by the authority
	// openssl genrsa -out serverkey.pem 512
	// openssl req -new -key serverkey.pem -out serverkey.csr -days 1095 -subj "/O=Example Inc/CN=localhost"
	// openssl x509 -req -in serverkey.csr -CA cacert.pem -CAkey cakey.pem -CAcreateserial -out servercert.pem -days 1095

	// # Generate a client key and certificate, signed by the authority
	// openssl genrsa -out clientkey.pem 512
	// openssl req -new -key clientkey.pem -out clientkey.csr -days 1095 -subj "/O=Example Inc/CN=Veneur client key"
	// openssl x509 -req -in clientkey.csr -CA cacert.pem -CAkey cakey.pem -CAcreateserial -out clientcert.pem -days 1095

	// # Generate another ca and sign the client key
	// openssl genrsa -out wrongcakey.pem 512
	// openssl req -new -x509 -key wrongcakey.pem -out wrongcacert.pem -days 1095 -subj "/O=Wrong Inc/CN=Wrong Certificate Authority"
	// openssl x509 -req -in clientkey.csr -CA wrongcacert.pem -CAkey wrongcakey.pem -CAcreateserial -out wrongclientcert.pem -days 1095

	pems := map[string]string{}
	pemFileNames := []string{
		"cacert.pem",
		"clientcert_correct.pem",
		"clientcert_wrong.pem",
		"clientkey.pem",
		"servercert.pem",
		"serverkey.pem",
	}
	for _, fileName := range pemFileNames {
		b, err := ioutil.ReadFile(filepath.Join("fixtures", fileName))
		if err != nil {
			return nil, err
		}
		pems[fileName] = string(b)
	}
	return pems, nil
}

// TestTCPConfig checks that invalid configurations are errors
func TestTCPConfig(t *testing.T) {
	config := localConfig()

	config.StatsdListenAddresses = []string{"tcp://invalid:invalid"}
	_, err := NewFromConfig(config)
	if err == nil {
		t.Error("invalid TCP address is a config error")
	}

	config.StatsdListenAddresses = []string{"tcp://localhost:8129"}
	config.TLSKey = "somekey"
	config.TLSCertificate = ""
	_, err = NewFromConfig(config)
	if err == nil {
		t.Error("key without certificate is a config error")
	}

	pems, err := readTestKeysCerts()
	if err != nil {
		t.Fatal("could not read test keys/certs:", err)
	}
	config.TLSKey = pems["serverkey.pem"]
	config.TLSCertificate = "somecert"
	_, err = NewFromConfig(config)
	if err == nil {
		t.Error("invalid key and certificate is a config error")
	}

	config.TLSKey = pems["serverkey.pem"]
	config.TLSCertificate = pems["servercert.pem"]
	_, err = NewFromConfig(config)
	if err != nil {
		t.Error("expected valid config")
	}
}

func sendTCPMetrics(addr string, tlsConfig *tls.Config, f *fixture) error {
	// TODO: attempt to ensure the accept goroutine opens the port before we attempt to connect
	// connect and send stats in two parts
	var conn net.Conn
	var err error
	if tlsConfig != nil {
		conn, err = tls.Dial("tcp", addr, tlsConfig)
	} else {
		conn, err = net.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()

	_, err = conn.Write([]byte("page.views:1|c\npage.views:1|c\n"))
	if err != nil {
		return err
	}
	err = conn.Close()
	if err != nil {
		return err
	}

	// check that the server received the stats; HACK: sleep to ensure workers process before flush
	time.Sleep(20 * time.Millisecond)

	if f.server.Workers[0].MetricsProcessedCount() < 1 {
		return fmt.Errorf("metrics were not processed")
	}

	return nil
}

func TestUDPMetrics(t *testing.T) {
	config := localConfig()
	config.NumWorkers = 1
	config.Interval = "60s"
	addr := fmt.Sprintf("127.0.0.1:%d", HTTPAddrPort)
	HTTPAddrPort++
	config.StatsdListenAddresses = []string{fmt.Sprintf("udp://%s", addr)}
	f := newFixture(t, config, nil, nil)
	defer f.Close()
	// Add a bit of delay to ensure things get listening
	time.Sleep(20 * time.Millisecond)

	conn, err := net.Dial("udp", addr)
	assert.NoError(t, err)
	defer conn.Close()

	conn.Write([]byte("foo.bar:1|c|#baz:gorch"))
	// Add a bit of delay to ensure things get processed
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int64(1), f.server.Workers[0].MetricsProcessedCount(), "worker processed metric")
}

func TestMultipleUDPSockets(t *testing.T) {
	config := localConfig()
	config.NumWorkers = 1
	config.Interval = "60s"
	addr1 := fmt.Sprintf("127.0.0.1:%d", HTTPAddrPort)
	HTTPAddrPort++
	addr2 := fmt.Sprintf("127.0.0.1:%d", HTTPAddrPort)
	HTTPAddrPort++
	config.StatsdListenAddresses = []string{fmt.Sprintf("udp://%s", addr1), fmt.Sprintf("udp://%s", addr2)}
	f := newFixture(t, config, nil, nil)
	defer f.Close()
	// Add a bit of delay to ensure things get listening
	time.Sleep(20 * time.Millisecond)

	conn, err := net.Dial("udp", addr1)
	assert.NoError(t, err)
	defer conn.Close()
	conn.Write([]byte("foo.bar:1|c|#baz:gorch"))

	conn2, err := net.Dial("udp", addr2)
	assert.NoError(t, err)
	defer conn2.Close()
	conn2.Write([]byte("foo.bar:1|c|#baz:gorch"))

	// Add a bit of delay to ensure things get processed
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int64(2), f.server.Workers[0].MetricsProcessedCount(), "worker processed metric")
}

func TestUDPMetricsSSF(t *testing.T) {
	config := localConfig()
	config.NumWorkers = 1
	config.Interval = "60s"
	addr := fmt.Sprintf("127.0.0.1:%d", HTTPAddrPort)
	config.SsfListenAddresses = []string{fmt.Sprintf("udp://%s", addr)}
	HTTPAddrPort++
	f := newFixture(t, config, nil, nil)
	defer f.Close()
	// listen delay
	time.Sleep(20 * time.Millisecond)

	conn, err := net.Dial("udp", addr)
	assert.NoError(t, err)
	defer conn.Close()

	testSample := &ssf.SSFSpan{}
	testMetric := &ssf.SSFSample{}
	testMetric.Name = "test.metric"
	testMetric.Metric = ssf.SSFSample_COUNTER
	testMetric.Value = 1
	testMetric.Tags = make(map[string]string)
	testMetric.Tags["tag"] = "tagValue"
	testSample.Metrics = append(testSample.Metrics, testMetric)

	packet, err := proto.Marshal(testSample)
	assert.NoError(t, err)
	conn.Write(packet)

	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int64(1), f.server.Workers[0].MetricsProcessedCount(), "worker processed metric")
}

func TestUNIXMetricsSSF(t *testing.T) {
	tdir, err := ioutil.TempDir("", "unixmetrics_ssf")
	require.NoError(t, err)
	defer os.RemoveAll(tdir)

	config := localConfig()
	config.NumWorkers = 1
	config.Interval = "60s"
	path := filepath.Join(tdir, "test.sock")
	config.SsfListenAddresses = []string{fmt.Sprintf("unix://%s", path)}
	HTTPAddrPort++
	f := newFixture(t, config, nil, nil)
	defer f.Close()
	// listen delay
	time.Sleep(20 * time.Millisecond)

	conn, err := net.Dial("unix", path)
	assert.NoError(t, err)
	defer conn.Close()

	testSpan := &ssf.SSFSpan{}
	testMetric := &ssf.SSFSample{}
	testMetric.Name = "test.metric"
	testMetric.Metric = ssf.SSFSample_COUNTER
	testMetric.Value = 1
	testMetric.Tags = make(map[string]string)
	testMetric.Tags["tag"] = "tagValue"
	testSpan.Metrics = append(testSpan.Metrics, testMetric)

	_, err = protocol.WriteSSF(conn, testSpan)
	if assert.NoError(t, err) {
		time.Sleep(20 * time.Millisecond)
		assert.Equal(t, int64(1), f.server.Workers[0].MetricsProcessedCount(), "worker processed metric")
	}

	_, err = protocol.WriteSSF(conn, testSpan)
	if assert.NoError(t, err) {
		time.Sleep(20 * time.Millisecond)
		assert.Equal(t, int64(2), f.server.Workers[0].MetricsProcessedCount(), "worker processed metric")
	}
}

func TestIgnoreLongUDPMetrics(t *testing.T) {
	config := localConfig()
	config.NumWorkers = 1
	config.MetricMaxLength = 31
	config.Interval = "60s"
	addr := fmt.Sprintf("127.0.0.1:%d", HTTPAddrPort)
	config.StatsdListenAddresses = []string{fmt.Sprintf("udp://%s", addr)}
	HTTPAddrPort++
	f := newFixture(t, config, nil, nil)
	defer f.Close()
	// Add a bit of delay to ensure things get listening
	time.Sleep(20 * time.Millisecond)

	conn, err := net.Dial("udp", addr)
	assert.NoError(t, err)
	defer conn.Close()

	// nb this metric is bad because it's too long based on the `MetricMaxLength`
	// we set above!
	conn.Write([]byte("foo.bar:1|c|#baz:gorch,long:tag,is:long"))
	// Add a bit of delay to ensure things get processed
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int64(0), f.server.Workers[0].processed, "worker did not process a metric")
}

// TestTCPMetrics checks that a server can accept metrics over a TCP socket.
func TestTCPMetrics(t *testing.T) {
	pems, err := readTestKeysCerts()
	if err != nil {
		t.Fatal("could not read test keys/certs:", err)
	}

	// all supported TCP connection modes
	serverConfigs := []struct {
		name                   string
		serverKey              string
		serverCertificate      string
		authorityCertificate   string
		expectedConnectResults [4]bool
	}{
		{"TCP", "", "", "", [4]bool{true, false, false, false}},
		{"encrypted", pems["serverkey.pem"], pems["servercert.pem"], "",
			[4]bool{false, true, true, true}},
		{"authenticated", pems["serverkey.pem"], pems["servercert.pem"], pems["cacert.pem"],
			[4]bool{false, false, false, true}},
	}

	// load all the various keys and certificates for the client
	trustServerCA := x509.NewCertPool()
	ok := trustServerCA.AppendCertsFromPEM([]byte(pems["cacert.pem"]))
	if !ok {
		t.Fatal("could not load server certificate")
	}
	wrongCert, err := tls.X509KeyPair(
		[]byte(pems["clientcert_wrong.pem"]), []byte(pems["clientkey.pem"]))
	if err != nil {
		t.Fatal("could not load wrong client cert/key:", err)
	}
	wrongConfig := &tls.Config{
		RootCAs:      trustServerCA,
		Certificates: []tls.Certificate{wrongCert},
	}
	correctCert, err := tls.X509KeyPair(
		[]byte(pems["clientcert_correct.pem"]), []byte(pems["clientkey.pem"]))
	if err != nil {
		t.Fatal("could not load correct client cert/key:", err)
	}
	correctConfig := &tls.Config{
		RootCAs:      trustServerCA,
		Certificates: []tls.Certificate{correctCert},
	}

	// all supported client configurations
	clientConfigs := []struct {
		name      string
		tlsConfig *tls.Config
	}{
		{"TCP", nil},
		{"TLS no cert", &tls.Config{RootCAs: trustServerCA}},
		{"TLS wrong cert", wrongConfig},
		{"TLS correct cert", correctConfig},
	}

	for _, serverConfig := range serverConfigs {
		config := localConfig()
		config.Interval = "60s"
		config.NumWorkers = 1
		// Use a unique port to avoid race with shutting down accept goroutine on Linux
		addr := fmt.Sprintf("localhost:%d", HTTPAddrPort)
		HTTPAddrPort++
		config.StatsdListenAddresses = []string{fmt.Sprintf("tcp://%s", addr)}
		config.TLSKey = serverConfig.serverKey
		config.TLSCertificate = serverConfig.serverCertificate
		config.TLSAuthorityCertificate = serverConfig.authorityCertificate
		f := newFixture(t, config, nil, nil)
		defer f.Close() // ensure shutdown if the test aborts

		// attempt to connect and send stats with each of the client configurations
		for i, clientConfig := range clientConfigs {
			expectedSuccess := serverConfig.expectedConnectResults[i]
			err := sendTCPMetrics(addr, clientConfig.tlsConfig, f)
			if err != nil {
				if expectedSuccess {
					t.Errorf("server config: '%s' client config: '%s' failed: %s",
						serverConfig.name, clientConfig.name, err.Error())
				} else {
					fmt.Printf("SUCCESS server config: '%s' client config: '%s' got expected error: %s\n",
						serverConfig.name, clientConfig.name, err.Error())
				}
			} else if !expectedSuccess {
				t.Errorf("server config: '%s' client config: '%s' worked; should fail!",
					serverConfig.name, clientConfig.name)
			} else {
				fmt.Printf("SUCCESS server config: '%s' client config: '%s'\n",
					serverConfig.name, clientConfig.name)
			}
		}

		f.Close()
	}
}

// TestHandleTCPGoroutineTimeout verifies that an idle TCP connection doesn't block forever.
func TestHandleTCPGoroutineTimeout(t *testing.T) {
	const readTimeout = 30 * time.Millisecond
	s := &Server{tcpReadTimeout: readTimeout, Workers: []*Worker{
		&Worker{PacketChan: make(chan samplers.UDPMetric, 1)},
	}}

	// make a real TCP connection ... to ourselves
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	acceptorDone := make(chan struct{})
	go func() {
		accepted, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}

		// after half the read timeout: send a stat; it should work
		time.Sleep(readTimeout / 2)
		_, err = accepted.Write([]byte("metric:42|g\n"))
		if err != nil {
			t.Error("expected Write to succeed:", err)
		}

		// read: returns when the connection is closed
		out, err := ioutil.ReadAll(accepted)
		if !(len(out) == 0 && err == nil) {
			t.Errorf("expected len(out)==0 (was %d) and err==nil (was %v)", len(out), err)
		}
		close(acceptorDone)
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// handleTCPGoroutine should not block forever: it will time outTest
	log.Printf("handling goroutine")
	s.handleTCPGoroutine(conn)
	<-acceptorDone

	// we should have received one metric
	packet := <-s.Workers[0].PacketChan
	if packet.Name != "metric" {
		t.Error("Expected packet for metric:", packet)
	}
}

// This is necessary until we can import
// github.com/sirupsen/logrus/test - it's currently failing due to dep
// insisting on pulling the repo in with its capitalized name.
//
// TODO: Revisit once https://github.com/golang/dep/issues/433 is fixed
func nullLogger() *logrus.Logger {
	logger := logrus.New()
	logger.Out = ioutil.Discard
	return logger
}

func TestCalculateTickerDelay(t *testing.T) {
	interval, _ := time.ParseDuration("10s")

	layout := "2006-01-02T15:04:05.000Z"
	str := "2014-11-12T11:45:26.371Z"
	theTime, _ := time.Parse(layout, str)
	delay := CalculateTickDelay(interval, theTime)
	assert.Equal(t, 3.629, delay.Seconds(), "Delay is incorrect")
}

// BenchmarkSendSSFUNIX sends b.N metrics to veneur and waits until
// all of them have been read (not processed).
func BenchmarkSendSSFUNIX(b *testing.B) {
	tdir, err := ioutil.TempDir("", "unixmetrics_ssf")
	require.NoError(b, err)
	defer os.RemoveAll(tdir)
	HTTPAddrPort++

	path := filepath.Join(tdir, "test.sock")
	// test the variables that have been renamed
	config := Config{
		DatadogAPIKey:          "apikey",
		DatadogAPIHostname:     "http://api",
		DatadogTraceAPIAddress: "http://trace",
		SsfListenAddresses:     []string{fmt.Sprintf("unix://%s", path)},

		// required or NewFromConfig fails
		Interval:     "10s",
		StatsAddress: "localhost:62251",
	}
	s, err := NewFromConfig(config)
	if err != nil {
		b.Fatal(err)
	}
	// Simulate a metrics worker:
	w := NewWorker(0, nil, nullLogger())
	s.Workers = []*Worker{w}
	go func() {
	}()
	defer close(w.QuitChan)
	// Simulate an incoming connection on the server:
	l, err := net.Listen("unix", path)
	require.NoError(b, err)
	defer l.Close()
	go func() {
		testSpan := &ssf.SSFSpan{}
		testMetric := &ssf.SSFSample{}
		testMetric.Name = "test.metric"
		testMetric.Metric = ssf.SSFSample_COUNTER
		testMetric.Value = 1
		testMetric.Tags = make(map[string]string)
		testMetric.Tags["tag"] = "tagValue"
		testSpan.Metrics = append(testSpan.Metrics, testMetric)

		conn, err := net.Dial("unix", path)
		require.NoError(b, err)
		defer conn.Close()
		for i := 0; i < b.N; i++ {
			_, err := protocol.WriteSSF(conn, testSpan)
			require.NoError(b, err)
		}
		conn.Close()
	}()
	sConn, err := l.Accept()
	require.NoError(b, err)
	go s.ReadSSFStreamSocket(sConn)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		<-w.PacketChan
	}
	close(s.shutdown)
}

// BenchmarkSendSSFUDP floods the veneur UDP socket with messages and
// and times how long it takes to read (not process) b.N metrics. This
// is almost an inversion of the SSFUNIX benchmark above, as UDP does
// lose packets and we don't want to loop forever.
func BenchmarkSendSSFUDP(b *testing.B) {
	addr := fmt.Sprintf("127.0.0.1:%d", HTTPAddrPort)
	HTTPAddrPort++
	// test the variables that have been renamed
	config := Config{
		DatadogAPIKey:          "apikey",
		DatadogAPIHostname:     "http://api",
		DatadogTraceAPIAddress: "http://trace",
		SsfListenAddresses:     []string{fmt.Sprintf("udp://%s", addr)},
		ReadBufferSizeBytes:    16 * 1024,
		TraceMaxLengthBytes:    900 * 1024,

		// required or NewFromConfig fails
		Interval:     "10s",
		StatsAddress: "localhost:62251",
	}
	s, err := NewFromConfig(config)
	if err != nil {
		b.Fatal(err)
	}
	pool := &sync.Pool{
		New: func() interface{} {
			return make([]byte, s.traceMaxLengthBytes)
		},
	}
	// Simulate listening for UDP SSF on the server:
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	require.NoError(b, err)
	l, err := NewSocket(udpAddr, s.RcvbufBytes, false)
	require.NoError(b, err)

	// Simulate a metrics worker:
	w := NewWorker(0, nil, nullLogger())
	s.Workers = []*Worker{w}

	go func() {
		testSpan := &ssf.SSFSpan{}
		testMetric := &ssf.SSFSample{}
		testMetric.Name = "test.metric"
		testMetric.Metric = ssf.SSFSample_COUNTER
		testMetric.Value = 1
		testMetric.Tags = make(map[string]string)
		testMetric.Tags["tag"] = "tagValue"
		testSpan.Metrics = append(testSpan.Metrics, testMetric)

		conn, err := net.Dial("udp", addr)
		require.NoError(b, err)
		defer conn.Close()
		for {
			select {
			case <-s.shutdown:
				return
			default:
			}
			packet, err := proto.Marshal(testSpan)
			assert.NoError(b, err)
			conn.Write(packet)
		}
	}()
	go s.ReadSSFPacketSocket(l, pool)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		<-w.PacketChan
	}
	l.Close()
	close(s.shutdown)
	return
}

func BenchmarkServerFlush(b *testing.B) {
	metricValues, _ := generateMetrics()
	config := localConfig()
	config.SsfListenAddresses = nil
	config.StatsdListenAddresses = nil
	f := newFixture(b, config, nil, nil)

	bhs, _ := blackhole.NewBlackholeMetricSink()
	f.server.metricSinks = []sinks.MetricSink{bhs}
	defer f.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, value := range metricValues {
			f.server.Workers[0].ProcessMetric(&samplers.UDPMetric{
				MetricKey: samplers.MetricKey{
					Name: "a.b.c",
					Type: "histogram",
				},
				Value:      value,
				Digest:     12345,
				SampleRate: 1.0,
				Scope:      samplers.LocalOnly,
			})
		}

		f.server.Flush(context.Background())
	}
}
