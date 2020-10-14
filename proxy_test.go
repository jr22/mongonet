package mongonet

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-test/deep"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type MyFactory struct {
}

func (myf *MyFactory) NewInterceptor(ps *ProxySession) (ProxyInterceptor, error) {
	return &MyInterceptor{ps}, nil
}

type MyInterceptor struct {
	ps *ProxySession
}

func (myi *MyInterceptor) Close() {
}
func (myi *MyInterceptor) TrackRequest(MessageHeader) {
}
func (myi *MyInterceptor) TrackResponse(MessageHeader) {
}

func (myi *MyInterceptor) CheckConnection() error {
	return nil
}

func (myi *MyInterceptor) CheckConnectionInterval() time.Duration {
	return 0
}

func (myi *MyInterceptor) InterceptClientToMongo(m Message) (Message, ResponseInterceptor, error) {
	switch mm := m.(type) {
	case *QueryMessage:
		if !NamespaceIsCommand(mm.Namespace) {
			return m, nil, nil
		}

		query, err := mm.Query.ToBSOND()
		if err != nil || len(query) == 0 {
			// let mongod handle error message
			return m, nil, nil
		}

		cmdName := strings.ToLower(query[0].Key)
		if cmdName != "ismaster" {
			return m, nil, nil
		}
		// remove client
		if idx := BSONIndexOf(query, "client"); idx >= 0 {
			query = append(query[:idx], query[idx+1:]...)
		}
		qb, err := SimpleBSONConvert(query)
		if err != nil {
			panic(err)
		}
		mm.Query = qb

		return mm, nil, nil
	}

	return m, nil, nil
}

func getTestClient(port int) (*mongo.Client, error) {
	opts := options.Client().ApplyURI(fmt.Sprintf("mongodb://localhost:%d", port))
	client, err := mongo.NewClient(opts)
	if err != nil {
		return nil, fmt.Errorf("cannot create a mongo client. err: %v", err)
	}
	return client, nil
}

func doFind(proxyPort, iteration int, shouldFail bool) error {
	client, err := getTestClient(proxyPort)
	if err != nil {
		return err
	}
	ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFunc()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("cannot connect to server. err: %v", err)
	}
	defer client.Disconnect(ctx)
	coll := client.Database("test").Collection(fmt.Sprintf("bar_%v", iteration))

	if err := coll.Drop(ctx); err != nil {
		return fmt.Errorf("failed to drop collection: %v", err)
	}

	docIn := bson.D{{"foo", int32(17)}}
	if _, err := coll.InsertOne(ctx, docIn); err != nil {
		return fmt.Errorf("can't insert: %v", err)
	}
	docOut := bson.D{}
	fopts := options.FindOne().SetProjection(bson.M{"_id": 0})
	err = coll.FindOne(ctx, bson.D{}, fopts).Decode(&docOut)
	if err != nil {
		if shouldFail {
			return nil
		}
		return fmt.Errorf("can't find: %v", err)
	}
	if shouldFail {
		return fmt.Errorf("expected find to fail but it didn't")
	}
	if len(docIn) != len(docOut) {
		return fmt.Errorf("docs don't match\n %v\n %v\n", docIn, docOut)
	}
	if diff := deep.Equal(docIn[0], docOut[0]); diff != nil {
		return fmt.Errorf("docs don't match: %v", diff)
	}
	return nil
}

func enableFailPoint(mongoPort int) error {
	client, err := getTestClient(mongoPort)
	if err != nil {
		return err
	}
	ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFunc()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("cannot connect to server. err: %v", err)
	}
	defer client.Disconnect(ctx)
	cmd := bson.D{
		{"configureFailPoint", "failCommand"},
		{"mode", "alwaysOn"},
		{"data", bson.D{
			{"failCommands", []string{"find"}},
			{"closeConnection", true},
		}},
	}
	return client.Database("admin").RunCommand(ctx, cmd).Err()
}

func disableFailPoint(mongoPort int) error {
	client, err := getTestClient(mongoPort)
	if err != nil {
		return err
	}
	ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFunc()
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("cannot connect to server. err: %v", err)
	}
	defer client.Disconnect(ctx)
	cmd := bson.D{
		{"configureFailPoint", "failCommand"},
		{"mode", "off"},
	}
	return client.Database("admin").RunCommand(ctx, cmd).Err()
}

func runFinds(proxyPort int, shouldFail bool, t *testing.T) int32 {
	var wg sync.WaitGroup
	var failing int32
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(iteration int) {
			defer wg.Done()
			err := doFind(proxyPort, iteration, shouldFail)
			if err != nil {
				t.Error(err)
				atomic.AddInt32(&failing, 1)
			}
		}(i)
	}
	wg.Wait()
	return failing
}

// backing mongo must be started with --setParameter enableTestCommands=1
func TestProxySanity(t *testing.T) {
	mongoPort := 30000
	proxyPort := 9900
	if os.Getenv("MONGO_PORT") != "" {
		mongoPort, _ = strconv.Atoi(os.Getenv("MONGO_PORT"))
	}
	if err := disableFailPoint(mongoPort); err != nil {
		t.Fatalf("failed to disable failpoint. err=%v", err)
		return
	}
	pc := NewProxyConfig("localhost", proxyPort, "localhost", mongoPort, "", "", "test proxy", true)
	pc.MongoSSLSkipVerify = true
	pc.InterceptorFactory = &MyFactory{}

	proxy, err := NewProxy(pc)
	if err != nil {
		panic(err)
	}

	proxy.InitializeServer()
	if ok, _, _ := proxy.OnSSLConfig(nil); !ok {
		panic("failed to call OnSSLConfig")
	}

	go proxy.Run()
	if conns := proxy.GetConnectionsCreated(); conns != 0 {
		t.Fatalf("expected connections created to equal 0 but was %v", conns)
	}
	failing := runFinds(proxyPort, false, t)
	if atomic.LoadInt32(&failing) > 0 {
		t.Fatalf("finds failures")
		return
	}

	if conns := proxy.GetConnectionsCreated(); conns != 5 {
		t.Fatalf("expected connections created to equal 5 but was %v", conns)
	}

	// run finds again to confirm connections are reused
	failing = runFinds(proxyPort, false, t)
	if atomic.LoadInt32(&failing) > 0 {
		t.Fatalf("finds failures")
		return
	}
	if conns := proxy.GetConnectionsCreated(); conns != 5 {
		t.Fatalf("expected connections created to equal 5 but was %v", conns)
	}

	// enable fail point - fail connections a bunch of times
	enableFailPoint(mongoPort)
	failing = runFinds(proxyPort, true, t)

	if atomic.LoadInt32(&failing) > 0 {
		t.Fatalf("finds failures")
		return
	}

	if conns := proxy.GetConnectionsCreated(); conns != 10 {
		t.Fatalf("expected connections created to equal 10 but was %v", conns)
	}
	// disable fail point - verify connections work again
	if err := disableFailPoint(mongoPort); err != nil {
		t.Fatalf("failed to disable failpoint. err=%v", err)
		return
	}

	failing = runFinds(proxyPort, false, t)
	if atomic.LoadInt32(&failing) > 0 {
		t.Fatalf("finds failures")
		return
	}

	if conns := proxy.GetConnectionsCreated(); conns != 15 {
		t.Fatalf("expected connections created to equal 15 but was %v", conns)
	}

}
