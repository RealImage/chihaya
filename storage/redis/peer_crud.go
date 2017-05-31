package redis

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	redigo "github.com/garyburd/redigo/redis"

	"github.com/RealImage/chihaya/bittorrent"
)

func decodePeerKey(pk string) bittorrent.Peer {
	return bittorrent.Peer{
		ID:   bittorrent.PeerIDFromString(string(pk[:20])),
		Port: binary.BigEndian.Uint16([]byte(pk[20:22])),
		IP:   net.IP(pk[22:]),
	}
}

//Adds an expiry to the set, that self deletes if not refreshed
func addPeer(s *peerStore, infoHash bittorrent.InfoHash, peerType string, pk serializedPeer) error {
	conn := s.connPool.Get()
	defer conn.Close()
	Key := fmt.Sprintf("%s:%s", peerType, infoHash)
	conn.Send("MULTI")
	conn.Send("ZADD", Key, time.Now().Unix(), pk)
	conn.Send("EXPIRE", Key, int(s.peerLifetime.Seconds()))
	_, err := conn.Do("EXEC")
	if err != nil {
		return err
	}
	return nil
}

func removePeers(s *peerStore, infoHash bittorrent.InfoHash, peerType string, pk serializedPeer) error {
	conn := s.connPool.Get()
	defer conn.Close()
	_, err := conn.Do("ZREM", fmt.Sprintf("%s:%s", peerType, infoHash), pk)
	if err != nil {
		return err
	}
	return nil
}

func getPeers(s *peerStore, infoHash bittorrent.InfoHash, peerType string, numWant int, peers []bittorrent.Peer, excludePeers bittorrent.Peer) ([]bittorrent.Peer, error) {
	conn := s.connPool.Get()
	defer conn.Close()
	Key := fmt.Sprintf("%s:%s", peerType, infoHash)
	peerList, err := redigo.Strings(conn.Do("ZRANGE", Key, 0, -1))
	if err != nil {
		return nil, err
	}
	for _, p := range peerList {
		if numWant == len(peers) {
			break
		}
		decodedPeer := decodePeerKey(p)
		if decodedPeer.Equal(excludePeers) {
			continue
		}
		peers = append(peers, decodedPeer)
	}
	return peers, nil
}

func pruneExpiredPeers(s *peerStore, infoHash bittorrent.InfoHash, peerType string) error {
	conn := s.connPool.Get()
	defer conn.Close()
	Key := fmt.Sprintf("%s:%s", peerType, infoHash)
	_, err := conn.Do("ZREMRANGEBYSCORE", Key, "-inf", fmt.Sprintf("(%d", time.Now().Add(-s.peerLifetime).Unix()))
	return err
}

func getPeersLength(s *peerStore, infoHash bittorrent.InfoHash, peerType string) (int, error) {
	conn := s.connPool.Get()
	defer conn.Close()
	return redigo.Int(conn.Do("ZCARD", fmt.Sprintf("%s:%s", peerType, infoHash)))
}
