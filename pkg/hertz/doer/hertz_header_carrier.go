package doer

import "github.com/cloudwego/hertz/pkg/protocol"

type hertzHeaderCarrier struct {
	header *protocol.RequestHeader
	keys   []string // cached keys to avoid allocation on repeated Keys() calls
}

func (c *hertzHeaderCarrier) Get(key string) string {
	return string(c.header.Peek(key))
}

func (c *hertzHeaderCarrier) Set(key string, value string) {
	c.header.Set(key, value)
}

func (c *hertzHeaderCarrier) Keys() []string {
	if c.keys != nil {
		return c.keys
	}

	if c.header.Len() == 0 {
		c.keys = nil
		return nil
	}

	c.keys = make([]string, 0, c.header.Len())
	c.header.VisitAll(func(key, value []byte) {
		c.keys = append(c.keys, string(key))
	})
	return c.keys
}
