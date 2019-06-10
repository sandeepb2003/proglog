package proglog

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"net"
	"os/user"
	"path/filepath"
	"reflect"
	"testing"

	api "github.com/travisjeffery/proglog/api/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestServer(t *testing.T) {
	for scenario, fn := range map[string]func(t *testing.T){
		"consume empty log fails":                             testConsumeEmpty,
		"produce/consume a message to/from the log succeeeds": testProduceConsume,
		"consume past log boundary fails":                     testConsumePastBoundary,
		"produce/consume stream succeeds":                     testProduceConsumeStream,
	} {
		t.Run(scenario, func(t *testing.T) { fn(t) })
	}
}

func testConsumeEmpty(t *testing.T) {
	client, _, teardown := testSetup(t)
	defer teardown()

	consume, err := client.Consume(context.Background(), &api.ConsumeRequest{
		Offset: 0,
	})
	if consume != nil {
		t.Fatalf("got consume: %v, want: nil", consume)
	}
	got, want := grpc.Code(err), grpc.Code(api.ErrOffsetOutOfRange{}.GRPCStatus().Err())
	if got != want {
		t.Fatalf("got code: %v, want: %v, err: %v", got, want, err)
	}
}

func testProduceConsume(t *testing.T) {
	client, _, teardown := testSetup(t)
	ctx := context.Background()
	defer teardown()

	want := &api.RecordBatch{
		Records: []*api.Record{{
			Value: []byte("hello world"),
		}},
	}

	produce, err := client.Produce(context.Background(), &api.ProduceRequest{
		RecordBatch: want,
	})
	check(t, err)

	consume, err := client.Consume(ctx, &api.ConsumeRequest{
		Offset: produce.FirstOffset,
	})
	check(t, err)
	equal(t, consume.RecordBatch, want)
}

func testConsumePastBoundary(t *testing.T) {
	client, _, teardown := testSetup(t)
	ctx := context.Background()
	defer teardown()

	produce, err := client.Produce(ctx, &api.ProduceRequest{
		RecordBatch: &api.RecordBatch{
			Records: []*api.Record{{
				Value: []byte("hello world"),
			}},
		},
	})
	check(t, err)

	consume, err := client.Consume(ctx, &api.ConsumeRequest{
		Offset: produce.FirstOffset + 1,
	})
	if consume != nil {
		t.Fatal("consume not nil")
	}
	got, want := grpc.Code(err), grpc.Code(api.ErrOffsetOutOfRange{}.GRPCStatus().Err())
	if got != want {
		t.Fatalf("got err: %v, want: %v", got, want)
	}
}

func testProduceConsumeStream(t *testing.T) {
	client, _, teardown := testSetup(t)
	ctx := context.Background()
	defer teardown()

	batches := []*api.RecordBatch{{
		Records: []*api.Record{{
			Value: []byte("first message"),
		}},
	}, {
		Records: []*api.Record{{
			Value: []byte("second message"),
		}},
	}}

	{
		stream, err := client.ProduceStream(ctx)
		check(t, err)

		for offset, batch := range batches {
			err = stream.Send(&api.ProduceRequest{
				RecordBatch: batch,
			})
			check(t, err)
			res, err := stream.Recv()
			check(t, err)
			if res.FirstOffset != uint64(offset) {
				t.Fatalf("got offset: %d, want: %d", res.FirstOffset, offset)
			}
		}

	}

	{
		stream, err := client.ConsumeStream(ctx, &api.ConsumeRequest{Offset: 0})
		check(t, err)

		for _, batch := range batches {
			res, err := stream.Recv()
			check(t, err)
			equal(t, res.RecordBatch, batch)
		}
	}
}

func testSetup(t *testing.T) (api.LogClient, *grpc.Server, func()) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	check(t, err)

	rawCACert, err := ioutil.ReadFile(caCrt)
	check(t, err)
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(rawCACert)

	clientCrt, err := tls.LoadX509KeyPair(clientCrt, clientKey)
	check(t, err)

	tlsCreds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clientCrt},
		RootCAs:      caCertPool,
	})

	cc, err := grpc.Dial(l.Addr().String(), grpc.WithTransportCredentials(tlsCreds))
	check(t, err)

	serverCrt, err := tls.LoadX509KeyPair(serverCrt, serverKey)
	check(t, err)

	tlsCreds = credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCrt},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caCertPool,
	})

	dir, err := ioutil.TempDir("", "server-test")
	check(t, err)

	config := &Config{
		CommitLog: &Log{Dir: dir},
	}
	server, err := NewAPI(config, grpc.Creds(tlsCreds))
	check(t, err)

	go func() {
		server.Serve(l)
	}()

	client := api.NewLogClient(cc)

	return client, server, func() {
		server.Stop()
		cc.Close()
		l.Close()
	}
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func equal(t *testing.T, got, want interface{}) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got: %v, want %v", got, want)
	}
}

var (
	caCrt     = configFile("ca.pem")
	serverCrt = configFile("server.pem")
	serverKey = configFile("server-key.pem")
	clientCrt = configFile("client.pem")
	clientKey = configFile("client-key.pem")
)

func configFile(filename string) string {
	u, err := user.Current()
	if err != nil {
		panic(err)
	}
	return filepath.Join(u.HomeDir, ".proglog", filename)
}
