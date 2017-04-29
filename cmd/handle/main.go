package main

import (
	"flag"
	"github.com/adamringhede/influxdb-ha/cluster"
	"log"
	"os"
	"strings"
	"github.com/adamringhede/influxdb-ha/service"
)

type controller struct {
	resolver *cluster.Resolver
}

func (c *controller) NotifyNewToken(token int, node *cluster.Node) {
	c.resolver.AddToken(token, node)
}

func (c *controller) NotifyRemovedToken(token int, node *cluster.Node) {
	c.resolver.RemoveToken(token)
}

func main() {
	bindAddr := flag.String("addr", "0.0.0.0", "IP addres for listening on cluster communication")
	bindPort := flag.Int("port", 8084, "Port for listening on cluster communication")
	bindClientAddr := flag.String("client-addr", "0.0.0.0", "IP addres for client http requests")
	bindClientPort := flag.Int("client-port", 8086, "Port for http requests")
	data := flag.String("data", ":28086", "InfluxDB database port")
	join := flag.String("join", "", "Comma seperated locations of other nodes")
	flag.Parse()

	clusterConfig := cluster.Config{
		BindAddr: *bindAddr,
		BindPort: *bindPort,
		DataLocation: *data,
	}
	handle := createClusterHandle(clusterConfig, join)

	resolver := cluster.NewResolver()
	handle.TokenDelegate = &controller{resolver}
	for _, token := range handle.LocalNode.Tokens {
		handle.TokenDelegate.NotifyNewToken(token, &handle.LocalNode.Node)
	}

	httpConfig := service.Config{
		BindAddr: *bindClientAddr,
		BindPort: *bindClientPort,
	}
	// TODO get partitioner from a database and listen for changes which should be reflected here.
	partitioner := cluster.NewPartitioner()
	partitioner.AddKey(cluster.PartitionKey{
		Database: "sharded",
		Measurement: "treasures",
		Tags: []string{"type"},
	})
	service.Start(resolver, partitioner, httpConfig)
}

func createClusterHandle(clusterConfig cluster.Config, join *string) *cluster.Handle {
	handle, err := cluster.NewHandle(clusterConfig)
	if err != nil {
		panic(err)
	}
	others := strings.Split(*join, ",")
	if len(others) > 0 && *join != "" {
		log.Printf("Joining: %s", *join)
		joinErr := handle.Join(others)
		if joinErr != nil {
			log.Println("Failed to join any other node")
		}
	}
	printHostname()
	return handle
}

func printHostname() {
	hostname, nameErr := os.Hostname()
	if nameErr != nil {
		panic(nameErr)
	}
	log.Printf("Hostname: %s", hostname)
}
