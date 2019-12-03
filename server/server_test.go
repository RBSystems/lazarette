package server

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/byuoitav/lazarette/lazarette"
	"github.com/byuoitav/lazarette/log"
	"github.com/byuoitav/lazarette/store/syncmapstore"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/ptypes"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

func newCache(tb testing.TB, logger *zap.Logger) *lazarette.Cache {
	store, err := syncmapstore.NewStore()
	if err != nil {
		tb.Fatalf("failed to create store: %v", err)
	}

	cache, err := lazarette.NewCache(store, logger)
	if err != nil {
		tb.Fatalf("failed to create cache: %v", err)
	}

	// make sure it's clean
	err = cache.Clean()
	if err != nil {
		tb.Fatalf("failed to clean cache: %v", err)
	}

	return cache
}

func startServer(tb testing.TB, cache *lazarette.Cache, grpcAddr, httpAddr string) *Server {
	tb.Helper()

	server := &Server{
		Cache: cache,
	}

	go func() {
		err := server.Serve(grpcAddr, httpAddr)
		if err != nil {
			tb.Fatalf("failed to serve: %v", err)
		}
	}()

	return server
}

func newGRPCClient(tb testing.TB, address string) lazarette.LazaretteClient {
	conn, err := grpc.Dial(address, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(2*time.Second))
	if err != nil {
		tb.Fatalf("failed to connect to server: %v", err)
	}

	return lazarette.NewLazaretteClient(conn)
}

func checkValueEqual(tb testing.TB, key string, expected, actual *lazarette.Value) {
	if !proto.Equal(expected, actual) {
		tb.Fatalf("values don't match for key %q:\n\texpected: %s\n\tactual: %s\n", key, expected.String(), actual.String())
	}
}

func TestMain(m *testing.M) {
	log.Config.Level.SetLevel(zap.ErrorLevel)
	os.Exit(m.Run())
}

func TestGRPCServer(t *testing.T) {
	ctx := context.Background()

	server := startServer(t, newCache(t, log.P.Named(":7777")), ":7777", "")
	client := newGRPCClient(t, "localhost:7777")
	defer server.Stop(ctx)

	kv := &lazarette.KeyValue{
		Key:       "ITB-1101-CP1",
		Timestamp: ptypes.TimestampNow(),
		Data:      []byte(`{"key": "value"}`),
	}

	t.Run("SetAndGet", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		_, err := client.Set(ctx, kv)
		if err != nil {
			t.Fatalf("failed to set %q: %v", kv.GetKey(), err)
		}

		nval, err := client.Get(ctx, &lazarette.Key{Key: kv.GetKey()})
		if err != nil {
			t.Fatalf("failed to get %q: %v", kv.GetKey(), err)
		}

		checkValueEqual(t, kv.GetKey(), &lazarette.Value{Timestamp: kv.GetTimestamp(), Data: kv.GetData()}, nval)
	})

	err := server.Cache.Clean()
	if err != nil {
		t.Fatalf("unable to clean cache between tests: %s", err)
	}

	t.Run("ReplicationGetInitialValues", func(t *testing.T) {
		// print logs
		log.Config.Level.SetLevel(zap.InfoLevel)
		defer log.Config.Level.SetLevel(zap.WarnLevel)

		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		server2 := startServer(t, newCache(t, log.P.Named(":45363")), ":45363", "")
		defer server2.Stop(ctx)

		// set a value in server1
		_, err := client.Set(ctx, kv)
		if err != nil {
			t.Fatalf("failed to set %q: %v", kv.GetKey(), err)
		}

		// have server2 replicate from server1
		client2 := newGRPCClient(t, "localhost:45363")
		go func() {
			_, err = client2.ReplicateWith(ctx, &lazarette.Replication{
				RemoteAddr: "localhost:7777",
				Prefix:     "ITB-1101-",
			})
			if err != nil {
				t.Logf("failed to replicate with server1: %s", err)
			}
		}()

		// let it replicate...
		time.Sleep(5 * time.Second)

		nval, err := client2.Get(ctx, &lazarette.Key{Key: kv.GetKey()})
		if err != nil {
			t.Fatalf("failed to get %q: %v", kv.GetKey(), err)
		}

		checkValueEqual(t, kv.GetKey(), &lazarette.Value{Timestamp: kv.GetTimestamp(), Data: kv.GetData()}, nval)
	})
}

func TestHttpServer(t *testing.T) {
	ctx := context.Background()

	server := startServer(t, newCache(t, log.P.Named(":7788")), "", ":7788")
	defer server.Stop(ctx)

	client := &http.Client{}

	kv := &lazarette.KeyValue{
		Key:       "ITB-1101-CP1",
		Timestamp: ptypes.TimestampNow(),
		Data:      []byte(`{"key": "value"}`),
	}

	t.Run("SetAndGet", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost:7788/cache/"+kv.GetKey(), bytes.NewBuffer(kv.GetData()))

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("failed to make set http request: %v", err)
		}
		defer resp.Body.Close()

		buf, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("failed to read response from set: %v", err)
		}

		expected := fmt.Sprintf("updated %s", kv.GetKey())
		if string(buf) != expected {
			t.Fatalf("invalid response on set:\nexpected: %s\ngot: %s\n", expected, string(buf))
		}

		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:7788/cache/"+kv.GetKey(), nil)

		resp2, err := client.Do(req)
		if err != nil {
			t.Fatalf("failed to make get http request: %v", err)
		}
		defer resp2.Body.Close()

		buf, err = ioutil.ReadAll(resp2.Body)
		if err != nil {
			t.Fatalf("failed to read response from get: %v", err)
		}

		tstamp, _ := time.Parse(time.RFC3339Nano, resp2.Header.Get("Last-Modified"))
		ptstamp, _ := ptypes.TimestampProto(tstamp)

		nval := &lazarette.Value{
			Timestamp: ptstamp,
			Data:      buf,
		}

		// reset the nanos (probably off)
		kv.GetTimestamp().Nanos = 0
		nval.Timestamp.Nanos = 0

		checkValueEqual(t, kv.GetKey(), &lazarette.Value{Data: kv.GetData(), Timestamp: kv.GetTimestamp()}, nval)
	})
}

func BenchmarkGRPCServer(t *testing.B) {
}

func BenchmarkHttpServer(b *testing.B) {
}
