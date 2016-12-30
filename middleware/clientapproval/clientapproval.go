// Package clientapproval implements a Hook that fails an Announce based on a
// whitelist or blacklist of BitTorrent client IDs.
package clientapproval

import (
	"context"
	"errors"

	"github.com/RealImage/chihaya/bittorrent"
	"github.com/RealImage/chihaya/middleware"
)

// ErrClientUnapproved is the error returned when a client's PeerID is invalid.
var ErrClientUnapproved = bittorrent.ClientError("unapproved client")

// Config represents all the values required by this middleware to validate
// peers based on their BitTorrent client ID.
type Config struct {
	Whitelist []string `yaml:"whitelist"`
	Blacklist []string `yaml:"blacklist"`
}

type hook struct {
	approved   map[bittorrent.ClientID]struct{}
	unapproved map[bittorrent.ClientID]struct{}
}

// NewHook returns an instance of the client approval middleware.
func NewHook(cfg Config) (middleware.Hook, error) {
	h := &hook{
		approved:   make(map[bittorrent.ClientID]struct{}),
		unapproved: make(map[bittorrent.ClientID]struct{}),
	}

	for _, cidString := range cfg.Whitelist {
		cidBytes := []byte(cidString)
		if len(cidBytes) != 6 {
			return nil, errors.New("client ID " + cidString + " must be 6 bytes")
		}
		var cid bittorrent.ClientID
		copy(cid[:], cidBytes)
		h.approved[cid] = struct{}{}
	}

	for _, cidString := range cfg.Blacklist {
		cidBytes := []byte(cidString)
		if len(cidBytes) != 6 {
			return nil, errors.New("client ID " + cidString + " must be 6 bytes")
		}
		var cid bittorrent.ClientID
		copy(cid[:], cidBytes)
		h.unapproved[cid] = struct{}{}
	}

	return h, nil
}

func (h *hook) HandleAnnounce(ctx context.Context, req *bittorrent.AnnounceRequest, resp *bittorrent.AnnounceResponse) (context.Context, error) {
	clientID := bittorrent.NewClientID(req.Peer.ID)

	if len(h.approved) > 0 {
		if _, found := h.approved[clientID]; !found {
			return ctx, ErrClientUnapproved
		}
	}

	if len(h.unapproved) > 0 {
		if _, found := h.unapproved[clientID]; found {
			return ctx, ErrClientUnapproved
		}
	}

	return ctx, nil
}

func (h *hook) HandleScrape(ctx context.Context, req *bittorrent.ScrapeRequest, resp *bittorrent.ScrapeResponse) (context.Context, error) {
	// Scrapes don't require any protection.
	return ctx, nil
}
