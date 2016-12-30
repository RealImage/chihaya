// Package udp implements a BitTorrent tracker via the UDP protocol as
// described in BEP 15.
package udp

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/RealImage/chihaya/bittorrent"
	"github.com/RealImage/chihaya/frontend"
	"github.com/RealImage/chihaya/frontend/udp/bytepool"
	"github.com/RealImage/chihaya/middleware"
)

func init() {
	prometheus.MustRegister(promResponseDurationMilliseconds)
	recordResponseDuration("action", nil, time.Second)
}

var promResponseDurationMilliseconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "chihaya_udp_response_duration_milliseconds",
		Help:    "The duration of time it takes to receive and write a response to an API request",
		Buckets: prometheus.ExponentialBuckets(9.375, 2, 10),
	},
	[]string{"action", "error"},
)

// recordResponseDuration records the duration of time to respond to a UDP
// Request in milliseconds .
func recordResponseDuration(action string, err error, duration time.Duration) {
	var errString string
	if err != nil {
		errString = err.Error()
	}

	promResponseDurationMilliseconds.
		WithLabelValues(action, errString).
		Observe(float64(duration.Nanoseconds()) / float64(time.Millisecond))
}

// Config represents all of the configurable options for a UDP BitTorrent
// Tracker.
type Config struct {
	Addr            string        `yaml:"addr"`
	PrivateKey      string        `yaml:"private_key"`
	MaxClockSkew    time.Duration `yaml:"max_clock_skew"`
	AllowIPSpoofing bool          `yaml:"allow_ip_spoofing"`
}

// Frontend holds the state of a UDP BitTorrent Frontend.
type Frontend struct {
	socket  *net.UDPConn
	closing chan struct{}
	wg      sync.WaitGroup

	logic frontend.TrackerLogic
	Config
}

// NewFrontend allocates a new instance of a Frontend.
func NewFrontend(logic frontend.TrackerLogic, cfg Config) *Frontend {
	return &Frontend{
		closing: make(chan struct{}),
		logic:   logic,
		Config:  cfg,
	}
}

// Stop provides a thread-safe way to shutdown a currently running Frontend.
func (t *Frontend) Stop() {
	close(t.closing)
	t.socket.SetReadDeadline(time.Now())
	t.wg.Wait()
}

// ListenAndServe listens on the UDP network address t.Addr and blocks serving
// BitTorrent requests until t.Stop() is called or an error is returned.
func (t *Frontend) ListenAndServe() error {
	udpAddr, err := net.ResolveUDPAddr("udp", t.Addr)
	if err != nil {
		return err
	}

	t.socket, err = net.ListenUDP("udp", udpAddr)
	if err != nil {
		return err
	}
	defer t.socket.Close()

	pool := bytepool.New(2048)

	for {
		// Check to see if we need to shutdown.
		select {
		case <-t.closing:
			return nil
		default:
		}

		// Read a UDP packet into a reusable buffer.
		buffer := pool.Get()
		t.socket.SetReadDeadline(time.Now().Add(time.Second))
		n, addr, err := t.socket.ReadFromUDP(buffer)
		if err != nil {
			pool.Put(buffer)
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				// A temporary failure is not fatal; just pretend it never happened.
				continue
			}
			return err
		}

		// We got nothin'
		if n == 0 {
			pool.Put(buffer)
			continue
		}

		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer pool.Put(buffer)

			if ip := addr.IP.To4(); ip != nil {
				addr.IP = ip
			}

			// Handle the request.
			start := time.Now()
			action, err := t.handleRequest(
				// Make sure the IP is copied, not referenced.
				Request{buffer[:n], append([]byte{}, addr.IP...)},
				ResponseWriter{t.socket, addr},
			)
			recordResponseDuration(action, err, time.Since(start))
		}()
	}
}

// Request represents a UDP payload received by a Tracker.
type Request struct {
	Packet []byte
	IP     net.IP
}

// ResponseWriter implements the ability to respond to a Request via the
// io.Writer interface.
type ResponseWriter struct {
	socket *net.UDPConn
	addr   *net.UDPAddr
}

// Write implements the io.Writer interface for a ResponseWriter.
func (w ResponseWriter) Write(b []byte) (int, error) {
	w.socket.WriteToUDP(b, w.addr)
	return len(b), nil
}

// handleRequest parses and responds to a UDP Request.
func (t *Frontend) handleRequest(r Request, w ResponseWriter) (actionName string, err error) {
	if len(r.Packet) < 16 {
		// Malformed, no client packets are less than 16 bytes.
		// We explicitly return nothing in case this is a DoS attempt.
		err = errMalformedPacket
		return
	}

	// Parse the headers of the UDP packet.
	connID := r.Packet[0:8]
	actionID := binary.BigEndian.Uint32(r.Packet[8:12])
	txID := r.Packet[12:16]

	// If this isn't requesting a new connection ID and the connection ID is
	// invalid, then fail.
	if actionID != connectActionID && !ValidConnectionID(connID, r.IP, time.Now(), t.MaxClockSkew, t.PrivateKey) {
		err = errBadConnectionID
		WriteError(w, txID, err)
		return
	}

	// Handle the requested action.
	switch actionID {
	case connectActionID:
		actionName = "connect"

		if !bytes.Equal(connID, initialConnectionID) {
			err = errMalformedPacket
			return
		}

		WriteConnectionID(w, txID, NewConnectionID(r.IP, time.Now(), t.PrivateKey))

	case announceActionID, announceV6ActionID:
		actionName = "announce"

		var req *bittorrent.AnnounceRequest
		req, err = ParseAnnounce(r, t.AllowIPSpoofing, actionID == announceV6ActionID)
		if err != nil {
			WriteError(w, txID, err)
			return
		}

		var resp *bittorrent.AnnounceResponse
		resp, err = t.logic.HandleAnnounce(context.Background(), req)
		if err != nil {
			WriteError(w, txID, err)
			return
		}

		WriteAnnounce(w, txID, resp, actionID == announceV6ActionID)

		go t.logic.AfterAnnounce(context.Background(), req, resp)

	case scrapeActionID:
		actionName = "scrape"

		var req *bittorrent.ScrapeRequest
		req, err = ParseScrape(r)
		if err != nil {
			WriteError(w, txID, err)
			return
		}

		ctx := context.WithValue(context.Background(), middleware.ScrapeIsIPv6Key, len(r.IP) == net.IPv6len)

		var resp *bittorrent.ScrapeResponse
		resp, err = t.logic.HandleScrape(ctx, req)
		if err != nil {
			WriteError(w, txID, err)
			return
		}

		WriteScrape(w, txID, resp)

		go t.logic.AfterScrape(context.Background(), req, resp)

	default:
		err = errUnknownAction
		WriteError(w, txID, err)
	}

	return
}
