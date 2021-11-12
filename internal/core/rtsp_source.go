package core

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/base"

	"github.com/aler9/rtsp-simple-server/internal/conf"
	"github.com/aler9/rtsp-simple-server/internal/logger"
)

const (
	rtspSourceRetryPause = 5 * time.Second
)

type rtspSourceParent interface {
	log(logger.Level, string, ...interface{})
	onSourceStaticSetReady(req pathSourceStaticSetReadyReq) pathSourceStaticSetReadyRes
	OnSourceStaticSetNotReady(req pathSourceStaticSetNotReadyReq)
}

type rtspSource struct {
	ur              string
	proto           conf.SourceProtocol
	anyPortEnable   bool
	fingerprint     string
	readTimeout     conf.StringDuration
	writeTimeout    conf.StringDuration
	readBufferCount int
	readBufferSize  int
	wg              *sync.WaitGroup
	parent          rtspSourceParent

	ctx       context.Context
	ctxCancel func()
}

func newRTSPSource(
	parentCtx context.Context,
	ur string,
	proto conf.SourceProtocol,
	anyPortEnable bool,
	fingerprint string,
	readTimeout conf.StringDuration,
	writeTimeout conf.StringDuration,
	readBufferCount int,
	readBufferSize int,
	wg *sync.WaitGroup,
	parent rtspSourceParent) *rtspSource {
	ctx, ctxCancel := context.WithCancel(parentCtx)

	s := &rtspSource{
		ur:              ur,
		proto:           proto,
		anyPortEnable:   anyPortEnable,
		fingerprint:     fingerprint,
		readTimeout:     readTimeout,
		writeTimeout:    writeTimeout,
		readBufferCount: readBufferCount,
		readBufferSize:  readBufferSize,
		wg:              wg,
		parent:          parent,
		ctx:             ctx,
		ctxCancel:       ctxCancel,
	}

	s.log(logger.Info, "started")

	s.wg.Add(1)
	go s.run()

	return s
}

func (s *rtspSource) close() {
	s.log(logger.Info, "stopped")
	s.ctxCancel()
}

func (s *rtspSource) log(level logger.Level, format string, args ...interface{}) {
	s.parent.log(level, "[rtsp source] "+format, args...)
}

func (s *rtspSource) run() {
	defer s.wg.Done()

	for {
		ok := func() bool {
			ok := s.runInner()
			if !ok {
				return false
			}

			select {
			case <-time.After(rtspSourceRetryPause):
				return true
			case <-s.ctx.Done():
				return false
			}
		}()
		if !ok {
			break
		}
	}

	s.ctxCancel()
}

func (s *rtspSource) runInner() bool {
	s.log(logger.Debug, "connecting")

	tlsConfig := &tls.Config{}
	if s.fingerprint != "" {
		tlsConfig.InsecureSkipVerify = true
		tlsConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			h := sha256.New()
			h.Write(cs.PeerCertificates[0].Raw)
			hstr := hex.EncodeToString(h.Sum(nil))
			fingerprintLower := strings.ToLower(s.fingerprint)

			if hstr != fingerprintLower {
				return fmt.Errorf("server fingerprint do not match: expected %s, got %s",
					fingerprintLower, hstr)
			}

			return nil
		}
	}

	c := &gortsplib.Client{
		Transport:       s.proto.Transport,
		TLSConfig:       tlsConfig,
		ReadTimeout:     time.Duration(s.readTimeout),
		WriteTimeout:    time.Duration(s.writeTimeout),
		ReadBufferCount: s.readBufferCount,
		ReadBufferSize:  s.readBufferSize,
		AnyPortEnable:   s.anyPortEnable,
		OnRequest: func(req *base.Request) {
			s.log(logger.Debug, "c->s %v", req)
		},
		OnResponse: func(res *base.Response) {
			s.log(logger.Debug, "s->c %v", res)
		},
	}

	u, err := base.ParseURL(s.ur)
	if err != nil {
		s.log(logger.Info, "ERR: %s", err)
		return true
	}

	err = c.Start(u.Scheme, u.Host)
	if err != nil {
		s.log(logger.Info, "ERR: %s", err)
		return true
	}

	readErr := make(chan error)
	go func() {
		readErr <- func() error {
			_, err = c.Options(u)
			if err != nil {
				return err
			}

			tracks, baseURL, _, err := c.Describe(u)
			if err != nil {
				return err
			}

			for _, t := range tracks {
				_, err := c.Setup(true, baseURL, t, 0, 0)
				if err != nil {
					panic(err)
				}
			}

			res := s.parent.onSourceStaticSetReady(pathSourceStaticSetReadyReq{
				Source: s,
				Tracks: c.Tracks(),
			})
			if res.Err != nil {
				return res.Err
			}

			s.log(logger.Info, "ready")

			defer func() {
				s.parent.OnSourceStaticSetNotReady(pathSourceStaticSetNotReadyReq{Source: s})
			}()

			c.OnPacketRTP = func(trackID int, payload []byte) {
				res.Stream.onPacketRTP(trackID, payload)
			}

			c.OnPacketRTCP = func(trackID int, payload []byte) {
				res.Stream.onPacketRTCP(trackID, payload)
			}

			_, err = c.Play(nil)
			if err != nil {
				return err
			}

			return c.Wait()
		}()
	}()

	select {
	case err := <-readErr:
		s.log(logger.Info, "ERR: %s", err)
		return true

	case <-s.ctx.Done():
		c.Close()
		<-readErr
		return false
	}
}

// onSourceAPIDescribe implements source.
func (*rtspSource) onSourceAPIDescribe() interface{} {
	return struct {
		Type string `json:"type"`
	}{"rtspSource"}
}