package appdash

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"sourcegraph.com/sourcegraph/appdash/internal/wire"
)

func TestCollectorServer(t *testing.T) {
	var (
		packets   []*wire.CollectPacket
		packetsMu sync.Mutex
	)
	mc := collectorFunc(func(span SpanID, anns ...Annotation) error {
		packetsMu.Lock()
		defer packetsMu.Unlock()
		packets = append(packets, newCollectPacket(span, anns))
		return nil
	})

	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("collector listening on %s", l.Addr())

	cs := NewServer(l, mc)
	go cs.Start()

	cc := &collectorT{t, NewRemoteCollector(l.Addr().String())}

	collectPackets := []*wire.CollectPacket{
		newCollectPacket(SpanID{1, 2, 3}, Annotations{{"k1", []byte("v1")}}),
		newCollectPacket(SpanID{2, 3, 4}, Annotations{{"k2", []byte("v2")}}),
	}
	for _, p := range collectPackets {
		cc.MustCollect(spanIDFromWire(p.Spanid), annotationsFromWire(p.Annotation)...)
	}
	if err := cc.Collector.(*RemoteCollector).Close(); err != nil {
		t.Error(err)
	}

	time.Sleep(20 * time.Millisecond)
	if !reflect.DeepEqual(packets, collectPackets) {
		t.Errorf("server collected %v, want %v", packets, collectPackets)
	}
}

func TestCollectorServer_stress(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	const (
		n             = 1000
		allowFailures = 0
		errorEvery    = 1000000
	)

	var (
		packets   = map[SpanID]struct{}{}
		packetsMu sync.Mutex
	)
	mc := collectorFunc(func(span SpanID, anns ...Annotation) error {
		packetsMu.Lock()
		defer packetsMu.Unlock()
		packets[span] = struct{}{}
		// log.Printf("Added %v", span)

		// Occasional errors, which should cause the client to
		// reconnect.
		if len(packets)%(errorEvery+1) == 0 {
			return errors.New("fake error")
		}
		return nil
	})

	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}

	cs := NewServer(l, mc)
	go cs.Start()

	cc := &collectorT{t, NewRemoteCollector(l.Addr().String())}
	// cc.Collector.(*RemoteCollector).Debug = true

	want := make(map[SpanID]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewRootSpanID()
		want[id] = struct{}{}
		cc.MustCollect(id)
	}
	if err := cc.Collector.(*RemoteCollector).Close(); err != nil {
		t.Error(err)
	}

	time.Sleep(20 * time.Millisecond)
	var missing []string
	for spanID := range want {
		if _, present := packets[spanID]; !present {
			missing = append(missing, fmt.Sprintf("span %v was not collected", spanID))
		}
	}
	if len(missing) > allowFailures {
		for _, missing := range missing {
			t.Error(missing)
		}
	}
}

func TestTLSCollectorServer(t *testing.T) {
	var numPackets int
	mc := collectorFunc(func(span SpanID, anns ...Annotation) error {
		numPackets++
		return nil
	})

	l, err := tls.Listen("tcp", "127.0.0.1:0", &localhostTLSConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("TLS collector listening on %s", l.Addr())

	cs := NewServer(l, mc)
	go cs.Start()

	cc := &collectorT{t, NewTLSRemoteCollector(l.Addr().String(), &localhostTLSConfig)}
	cc.MustCollect(SpanID{1, 2, 3})
	cc.MustCollect(SpanID{2, 3, 4})
	if err := cc.Collector.(*RemoteCollector).Close(); err != nil {
		t.Error(err)
	}

	time.Sleep(20 * time.Millisecond)
	if want := 2; numPackets != want {
		t.Errorf("server collected %d packets, want %d", numPackets, want)
	}
}

func TestChunkedCollector(t *testing.T) {
	var packets []*wire.CollectPacket
	mc := collectorFunc(func(span SpanID, anns ...Annotation) error {
		packets = append(packets, newCollectPacket(span, anns))
		return nil
	})

	cc := &ChunkedCollector{
		Collector:   mc,
		MinInterval: time.Millisecond * 10,
	}
	cc.Collect(SpanID{1, 2, 3}, Annotation{"k1", []byte("v1")})
	cc.Collect(SpanID{1, 2, 3}, Annotation{"k2", []byte("v2")})
	cc.Collect(SpanID{2, 3, 4}, Annotation{"k3", []byte("v3")})
	cc.Collect(SpanID{1, 2, 3}, Annotation{"k4", []byte("v4")})

	// Check before the MinInterval has elapsed.
	if len(packets) != 0 {
		t.Errorf("before MinInterval: got len(packets) == %d, want 0", len(packets))
	}

	time.Sleep(cc.MinInterval * 2)

	// Check after the MinInterval has elapsed.
	want := []*wire.CollectPacket{
		newCollectPacket(SpanID{1, 2, 3}, Annotations{{"k1", []byte("v1")}, {"k2", []byte("v2")}, {"k4", []byte("v4")}}),
		newCollectPacket(SpanID{2, 3, 4}, Annotations{{"k3", []byte("v3")}}),
	}
	if !reflect.DeepEqual(packets, want) {
		t.Errorf("after MinInterval: got packets == %v, want %v", packets, want)
	}

	// Check that Stop stops it.
	lenBeforeStop := len(packets)
	cc.Stop()
	cc.Collect(SpanID{1, 2, 3}, Annotation{"k5", []byte("v5")})
	time.Sleep(cc.MinInterval * 2)
	if len(packets) != lenBeforeStop {
		t.Errorf("after Stop: got len(packets) == %d, want %d", len(packets), lenBeforeStop)
	}
}

// collectorFunc implements the Collector interface by calling the function.
type collectorFunc func(SpanID, ...Annotation) error

// Collect implements the Collector interface by calling the function itself.
func (c collectorFunc) Collect(id SpanID, as ...Annotation) error {
	return c(id, as...)
}

type collectorT struct {
	t *testing.T
	Collector
}

func (s collectorT) MustCollect(id SpanID, as ...Annotation) {
	if err := s.Collector.Collect(id, as...); err != nil {
		s.t.Fatalf("Collect(%+v, %v): %s", id, as, err)
	}
}

var localhostTLSConfig tls.Config

func init() {
	cert, err := tls.X509KeyPair(localhostCert, localhostKey)
	if err != nil {
		panic(fmt.Sprintf("localhostTLSConfig: %v", err))
	}
	localhostTLSConfig.Certificates = []tls.Certificate{cert}

	certPool := x509.NewCertPool()
	if ok := certPool.AppendCertsFromPEM(localhostCert); !ok {
		panic("AppendCertsFromPEM: !ok")
	}
	localhostTLSConfig.RootCAs = certPool

	localhostTLSConfig.BuildNameToCertificate()
}

// localhostCert is a PEM-encoded TLS cert with SAN IPs
// "127.0.0.1" and "[::1]", expiring at the last second of 2049 (the end
// of ASN.1 time).
// generated from src/crypto/tls:
// go run generate_cert.go  --rsa-bits 512 --host 127.0.0.1,::1,example.com --ca --start-date "Jan 1 00:00:00 1970" --duration=1000000h
var localhostCert = []byte(`-----BEGIN CERTIFICATE-----
MIIBdzCCASOgAwIBAgIBADALBgkqhkiG9w0BAQUwEjEQMA4GA1UEChMHQWNtZSBD
bzAeFw03MDAxMDEwMDAwMDBaFw00OTEyMzEyMzU5NTlaMBIxEDAOBgNVBAoTB0Fj
bWUgQ28wWjALBgkqhkiG9w0BAQEDSwAwSAJBAN55NcYKZeInyTuhcCwFMhDHCmwa
IUSdtXdcbItRB/yfXGBhiex00IaLXQnSU+QZPRZWYqeTEbFSgihqi1PUDy8CAwEA
AaNoMGYwDgYDVR0PAQH/BAQDAgCkMBMGA1UdJQQMMAoGCCsGAQUFBwMBMA8GA1Ud
EwEB/wQFMAMBAf8wLgYDVR0RBCcwJYILZXhhbXBsZS5jb22HBH8AAAGHEAAAAAAA
AAAAAAAAAAAAAAEwCwYJKoZIhvcNAQEFA0EAAoQn/ytgqpiLcZu9XKbCJsJcvkgk
Se6AbGXgSlq+ZCEVo0qIwSgeBqmsJxUu7NCSOwVJLYNEBO2DtIxoYVk+MA==
-----END CERTIFICATE-----`)

// localhostKey is the private key for localhostCert.
var localhostKey = []byte(`-----BEGIN RSA PRIVATE KEY-----
MIIBPAIBAAJBAN55NcYKZeInyTuhcCwFMhDHCmwaIUSdtXdcbItRB/yfXGBhiex0
0IaLXQnSU+QZPRZWYqeTEbFSgihqi1PUDy8CAwEAAQJBAQdUx66rfh8sYsgfdcvV
NoafYpnEcB5s4m/vSVe6SU7dCK6eYec9f9wpT353ljhDUHq3EbmE4foNzJngh35d
AekCIQDhRQG5Li0Wj8TM4obOnnXUXf1jRv0UkzE9AHWLG5q3AwIhAPzSjpYUDjVW
MCUXgckTpKCuGwbJk7424Nb8bLzf3kllAiA5mUBgjfr/WtFSJdWcPQ4Zt9KTMNKD
EUO0ukpTwEIl6wIhAMbGqZK3zAAFdq8DD2jPx+UJXnh0rnOkZBzDtJ6/iN69AiEA
1Aq8MJgTaYsDQWyU/hDq5YkDJc9e9DSCvUIzqxQWMQE=
-----END RSA PRIVATE KEY-----`)

func BenchmarkChunkedCollector500(b *testing.B) {
	cc := &ChunkedCollector{
		Collector: collectorFunc(func(span SpanID, anns ...Annotation) error {
			return nil
		}),
		MinInterval: time.Millisecond * 10,
	}
	var x ID
	for i := 0; i < b.N; i++ {
		for c := 0; c < 500; c++ {
			x++
			err := cc.Collect(SpanID{x, x + 1, x + 2})
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
