package balancer

import (
	"errors"
	"hash/fnv"
)

type IPHash struct {
	BaseBalancer
}

func NewIPHash() *IPHash {
	return &IPHash{}
}

func (i *IPHash) NextForIP(ip string) (*Backend, error) {
	healthy := i.GetHealthy()
	if len(healthy) == 0 {
		return nil, errors.New("no healthy backends")
	}

	h := fnv.New32a()
	h.Write([]byte(ip))
	hash := h.Sum32()

	index := hash % uint32(len(healthy))
	selected := healthy[index]
	selected.IncrConn()
	return selected, nil
}

// NextForKey satisfies the KeyedBalancer capability by hashing an arbitrary
// routing key onto the healthy backend set. For IPHash the key is expected to be
// the client IP, so this is equivalent to NextForIP.
func (i *IPHash) NextForKey(key string) (*Backend, error) {
	return i.NextForIP(key)
}

func (i *IPHash) Next() (*Backend, error) {
	return nil, errors.New("IPHash requires client IP, use NextForIP instead")
}
