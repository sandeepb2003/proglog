package proglog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"log"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_auth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	"github.com/hashicorp/serf/serf"
	api "github.com/travisjeffery/proglog/api/v1"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

var _ api.LogServer = (*grpcServer)(nil)

type Config struct {
	SerfConfig *serf.Config
	TLSConfig  *TLSConfig
	RPCAddr    string
}

type TLSConfig struct {
	CACert                string
	ClientCert, ClientKey string
}

func NewAPI(log logger, opts ...grpc.ServerOption) (*grpc.Server, error) {
	opts = append(opts, grpc.StreamInterceptor(grpc_middleware.ChainStreamServer(
		grpc_auth.StreamServerInterceptor(auth),
	)), grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
		grpc_auth.UnaryServerInterceptor(auth),
	)))
	gsrv := grpc.NewServer(opts...)
	srv, err := newgrpcServer(log)
	if err != nil {
		return nil, err
	}
	api.RegisterLogServer(gsrv, srv)
	return gsrv, nil
}

func newgrpcServer(log logger) (srv *grpcServer, err error) {
	srv = &grpcServer{
		log: log,
	}
	err = srv.setupSerf()
	if err != nil {
		return nil, err
	}
	err = srv.setupTLS()
	if err != nil {
		return nil, err
	}
	return srv, nil
}

type grpcServer struct {
	Config   *Config
	log      logger
	serf     *serf.Serf
	events   chan serf.Event
	tlsCreds credentials.TransportCredentials
}

func (s *grpcServer) setupSerf() (err error) {
	conf := s.Config.SerfConfig
	conf.Init()
	conf.Tags[rpcAddrKey] = s.Config.RPCAddr
	s.events = make(chan serf.Event)
	conf.EventCh = s.events
	s.serf, err = serf.Create(conf)
	if err != nil {
		return err
	}
	go s.eventHandler()
	return nil
}

func (s *grpcServer) setupTLS() (err error) {
	clientCrt, err := tls.LoadX509KeyPair(
		s.Config.TLSConfig.ClientCert,
		s.Config.TLSConfig.ClientKey,
	)
	if err != nil {
		return err
	}

	rawCACert, err := ioutil.ReadFile(s.Config.TLSConfig.CACert)
	if err != nil {
		return err
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(rawCACert)

	s.tlsCreds = credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clientCrt},
		RootCAs:      caCertPool,
	})

	return nil
}

func (s *grpcServer) Produce(ctx context.Context, req *api.ProduceRequest) (*api.ProduceResponse, error) {
	offset, err := s.log.AppendBatch(req.RecordBatch)
	if err != nil {
		return nil, err
	}
	return &api.ProduceResponse{FirstOffset: offset}, s.replicateProduce(ctx, req)
}

func (s *grpcServer) replicateProduce(ctx context.Context, req *api.ProduceRequest) error {
	g, ctx := errgroup.WithContext(ctx)
	for _, member := range s.serf.Members() {
		server := decodeParts(member)
		if server.rpcAddr == s.Config.RPCAddr {
			// ignore the member of the current server
			continue
		}
		g.Go(func() error {
			// TODO(tj): optimize this

			cc, err := grpc.Dial(server.rpcAddr, grpc.WithTransportCredentials(s.tlsCreds))
			if err != nil {
				return err
			}
			defer cc.Close()

			client := api.NewLogClient(cc)

			_, err = client.Produce(ctx, req)
			if err != nil {
				return err
			}

			return nil
		})
	}
	return g.Wait()
}

func (s *grpcServer) Consume(ctx context.Context, req *api.ConsumeRequest) (*api.ConsumeResponse, error) {
	batch, err := s.log.ReadBatch(req.Offset)
	if err != nil {
		return nil, err
	}
	return &api.ConsumeResponse{RecordBatch: batch}, nil
}

func (s *grpcServer) ConsumeStream(req *api.ConsumeRequest, stream api.Log_ConsumeStreamServer) error {
	for {
		res, err := s.Consume(stream.Context(), req)
		if err != nil {
			return err
		}
		if err = stream.Send(res); err != nil {
			return err
		}
		req.Offset++
	}
}

func (s *grpcServer) ProduceStream(stream api.Log_ProduceStreamServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		res, err := s.Produce(stream.Context(), req)
		if err != nil {
			return err
		}
		if err = stream.Send(res); err != nil {
			return err
		}
	}
}

type logger interface {
	AppendBatch(*api.RecordBatch) (uint64, error)
	ReadBatch(uint64) (*api.RecordBatch, error)
}

func auth(ctx context.Context) (context.Context, error) {
	peer, ok := peer.FromContext(ctx)
	if ok {
		tlsInfo := peer.AuthInfo.(credentials.TLSInfo)
		addr := peer.Addr.String()
		username := tlsInfo.State.VerifiedChains[0][0].Subject.CommonName
		log.Printf("auth: %s: %s", addr, username)
	}
	return ctx, nil
}
