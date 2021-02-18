package http2

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

var clientStreamPool = sync.Pool{
	New: func() interface{} {
		return &ClientStream{}
	},
}

type ClientStream struct {
	id    uint32
	state StreamState

	windowSize uint32

	ch    chan *Frame
	errch chan error
}

func acquireClientStream(id uint32) *ClientStream {
	strm := clientStreamPool.Get().(*ClientStream)

	strm.id = id
	strm.state = StateIdle
	strm.ch = make(chan *Frame, 1)
	strm.errch = make(chan error, 1)

	return strm
}

func releaseClientStream(strm *ClientStream) {
	close(strm.ch)
	streamPool.Put(strm)
}

type Client struct {
	hp     *HPACK
	nextID uint32
	c      net.Conn
	st     *Settings
	wch    chan *Frame
	rch    chan *Frame
	closer chan struct{}
	strms  []*ClientStream
}

func NewClient() *Client {
	return &Client{
		nextID: 1,
		hp:     AcquireHPACK(),
		st:     AcquireSettings(),
		wch:    make(chan *Frame, 1024),
		rch:    make(chan *Frame, 1024),
		closer: make(chan struct{}, 1),
		strms:  make([]*ClientStream, 0, 128),
	}
}

// TODO: checkout https://github.com/golang/net/blob/4acb7895a057/http2/transport.go#L570
func ConfigureClient(c *fasthttp.HostClient) error {
	c2 := NewClient()
	err := c2.Dial(c.Addr, &tls.Config{
		MinVersion: tls.VersionTLS12,
		MaxVersion: tls.VersionTLS13,
		NextProtos: []string{"h2"},
	})
	if err != nil {
		return err
	}

	// TODO: Checkout the tlsconfig....

	c.Transport = c2.Do

	return nil
}

func (c *Client) Dial(addr string, tlsConfig *tls.Config) error {
	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return err
	}

	err = conn.Handshake()
	if err != nil {
		conn.Close()
		return err
	}

	c.c = conn

	err = c.Handshake()
	if err != nil {
		conn.Close()
		return err
	}

	go c.readLoop()
	go c.writeLoop()

	c.writeWindowUpdate(c.st.MaxWindowSize())

	return nil
}

func (c *Client) Handshake() error {
	conn := c.c

	_, err := conn.Write(http2Preface)
	if err != nil {
		return err
	}

	fr := AcquireFrame()
	defer ReleaseFrame(fr)

	c.st = AcquireSettings()
	c.st.WriteFrame(fr)

	_, err = fr.WriteTo(conn)
	return err
}

func (c *Client) getStream(id uint32) *ClientStream {
	for _, strm := range c.strms {
		if strm.id == id {
			return strm
		}
	}
	return nil
}

func (c *Client) readLoop() {
	defer c.c.Close()
	br := bufio.NewReader(c.c)

	for {
		fr := AcquireFrame()

		_, err := fr.ReadFrom(br)
		if err != nil {
			log.Println(err)
			return
		}

		if fr.Stream() == 0 {
			ReleaseFrame(fr)
			continue
		}

		strm := c.getStream(fr.Stream())
		if strm != nil {
			strm.ch <- fr
		} else {
			log.Println(fr.Stream(), "not found")
			ReleaseFrame(fr)
		}
	}
}

func (c *Client) writeLoop() {
	defer c.c.Close()
	bw := bufio.NewWriter(c.c)

	for {
		select {
		case fr, ok := <-c.wch:
			if !ok {
				return
			}

			_, err := fr.WriteTo(bw)
			if err == nil {
				err = bw.Flush()
			}
			if err != nil {
				strm := c.getStream(fr.Stream())
				if strm != nil {
					strm.errch <- err
				}
			}

			ReleaseFrame(fr)
		case <-time.After(time.Second * 5):
		// TODO: PING ...
		case <-c.closer:
			return
		}
	}
}

func (c *Client) Do(req *fasthttp.Request, res *fasthttp.Response) error {
	strm := acquireClientStream(c.nextID)
	c.strms = append(c.strms, strm)

	atomic.AddUint32(&c.nextID, 2)

	c.writeRequest(strm, req)
	err := c.readResponse(strm, res)

	releaseClientStream(strm)

	return err
}

func (c *Client) writeRequest(strm *ClientStream, req *fasthttp.Request) {
	// TODO: GET requests can have body too
	// TODO: Send continuation if needed
	noBody := req.Header.IsHead() || req.Header.IsGet()

	fr := AcquireFrame()
	h := AcquireHeaders()
	hf := AcquireHeaderField()

	defer ReleaseFrame(fr)
	defer ReleaseHeaders(h)
	defer ReleaseHeaderField(hf)

	hf.SetBytes(strAuthority, req.Host())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	hf.SetBytes(strMethod, req.Header.Method())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	hf.SetBytes(strScheme, req.URI().Scheme())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	hf.SetBytes(strPath, req.URI().Path())
	h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)

	req.Header.VisitAll(func(k, v []byte) {
		hf.SetBytes(bytes.ToLower(k), v)
		h.rawHeaders = c.hp.AppendHeader(h.rawHeaders, hf)
	})

	h.SetPadding(false)
	h.SetEndStream(!noBody)
	h.SetEndHeaders(true)

	h.WriteFrame(fr)
	fr.SetStream(strm.id)

	c.writeFrame(fr)
}

func (c *Client) writeFrame(fr *Frame) {
	c.wch <- fr
}

func (c *Client) readResponse(strm *ClientStream, res *fasthttp.Response) error {
	var (
		fr *Frame
		err error
	)

	for {
		select {
		case fr = <-strm.ch:
		case err = <-strm.errch:
			return err
		}

		fmt.Println(fr.Stream(), fr.Type())

		var isEnd bool
		switch fr.Type() {
		case FrameHeaders:
			isEnd, err = c.handleHeaders(fr, res)
		case FrameData:
			isEnd = true
			err = c.handleData(fr, res)
		}

		if isEnd {
			strm.state++
			break
		}
	}

	return err
}

func (c *Client) writeWindowUpdate(update uint32) {
	fr := AcquireFrame()
	wu := AcquireWindowUpdate()
	wu.SetIncrement(update)

	wu.WriteFrame(fr)
	c.writeFrame(fr)

	ReleaseWindowUpdate(wu)
}

func (c *Client) handleHeaders(fr *Frame, res *fasthttp.Response) (bool, error) {
	h := AcquireHeaders()
	hf := AcquireHeaderField()

	defer ReleaseHeaders(h)
	defer ReleaseHeaderField(hf)

	err := h.ReadFrame(fr)
	if err != nil {
		return false, err
	}

	b := fr.Payload()
	for len(b) > 0 {
		b, err = c.hp.Next(hf, b)
		if err != nil {
			return false, err
		}

		if hf.IsPseudo() {
			if hf.key[1] == 's' { // status
				n, err := strconv.ParseInt(hf.Value(), 10, 64)
				if err != nil {
					return false, err
				}

				res.SetStatusCode(int(n))
				continue
			}
		}

		res.Header.AddBytesKV(hf.KeyBytes(), hf.ValueBytes())
	}

	return h.EndStream(), err
}

func (c *Client) handleData(fr *Frame, res *fasthttp.Response) error {
	dfr := AcquireData()
	defer ReleaseData(dfr)

	err := dfr.ReadFrame(fr)
	if err != nil {
		return err
	}

	res.SetBody(dfr.Data())

	return nil
}