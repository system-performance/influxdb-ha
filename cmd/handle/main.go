package main

import (
	"context"
	"flag"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/adamringhede/influxdb-ha/cluster"
	"github.com/adamringhede/influxdb-ha/service"
	"github.com/adamringhede/influxdb-ha/syncing"
	"github.com/coreos/etcd/clientv3"
)

func main() {
	bindClientAddr := flag.String("client-addr", "0.0.0.0", "IP addres for client http requests")
	bindClientPort := flag.Int("client-port", 8086, "Port for http requests")
	data := flag.String("data", ":28086", "InfluxDB database port")
	etcdEndpoints := flag.String("etcd", "", "Comma seperated locations of etcd nodes")
	flag.Parse()

	c, etcdErr := clientv3.New(clientv3.Config{
		Endpoints:   strings.Split(*etcdEndpoints, ","),
		DialTimeout: 5 * time.Second,
	})
	handleErr(etcdErr)

	// TODO Only default to hostname, prefer using a configurable id
	// Maybe create a unique ID upon first time, save it on the local file system and reuse it next time.
	nodeName, hostErr := os.Hostname()
	handleErr(hostErr)

	// Setup storage components
	nodeStorage := cluster.NewEtcdNodeStorage(c)
	tokenStorage := cluster.NewEtcdTokenStorageWithClient(c)
	hintsStorage := cluster.NewEtcdHintStorage(c, nodeName)
	settingsStorage := cluster.NewEtcdSettingsStorage(c)
	partitionKeyStorage := cluster.NewEtcdPartitionKeyStorage(c)
	recoveryStorage := cluster.NewLocalRecoveryStorage("./", hintsStorage)

	nodeCollection, err := cluster.NewSyncedNodeCollection(nodeStorage)
	handleErr(err)

	localNode, nodeErr := nodeStorage.Get(nodeName)
	handleErr(nodeErr)
	isNew := localNode == nil
	if localNode == nil {
		localNode = &cluster.Node{}
		localNode.Name = nodeName
	}
	localNode.DataLocation = *data
	handleErr(nodeStorage.Save(localNode))

	if !isNew {
		// Check if recovering (others nodes hold data)
		selfHints, err := hintsStorage.GetByTarget(localNode.Name)
		handleErr(err)
		if len(selfHints) != 0 {
			cluster.WaitUntilRecoveredWithCallback(hintsStorage, localNode.Name, func() {
				// It may be better instead to emit an event like finishedRecovery to some
				// thing that manages the state.
				localNode.Status = cluster.NodeStatusUp
				nodeStorage.Save(localNode)
			})
		}
	}

	defaultReplicationFactor, err := settingsStorage.GetDefaultReplicationFactor()
	handleErr(err)

	resolver := cluster.NewResolverWithNodes(nodeCollection)
	_, err = cluster.NewResolverSyncer(resolver, tokenStorage, nodeCollection)
	handleErr(err)
	resolver.ReplicationFactor = defaultReplicationFactor

	_, importWQ := startImporter(c, resolver, *localNode)

	nodeStorage.OnRemove(func(removedNode cluster.Node) {
		// Distribute tokens to other nodes
		nodes := []string{}
		tokenGroups := map[string][]int{}
		for name := range nodeCollection.GetAll() {
			if name != removedNode.Name {
				if _, ok := tokenGroups[name]; !ok {
					nodes = append(nodes, name)
					tokenGroups[name] = []int{}
				}
			}
		}
		tokensMap, err := tokenStorage.Get()
		if err != nil {
			return
		}
		var i int
		for token, nodeName := range tokensMap {
			if nodeName != removedNode.Name {
				selectedNode := nodes[i%len(nodes)]
				tokenGroups[selectedNode] = append(tokenGroups[selectedNode], token)
				tokenGroups[selectedNode] = append(tokenGroups[selectedNode], resolver.ReverseSecondaryLookup(token)...)
				i++
			}
		}
		for nodeName, tokens := range tokenGroups {
			importWQ.Push(nodeName, syncing.ReliableImportPayload{Tokens: tokens})
		}
	})

	partitioner, err := cluster.NewSyncedPartitioner(partitionKeyStorage)
	handleErr(err)
	partitioner.AddKey(cluster.PartitionKey{
		Database:    "sharded",
		Measurement: "treasures",
		Tags:        []string{"type"},
	})

	go (func() {
		for rf := range settingsStorage.WatchDefaultReplicationFactor() {
			resolver.ReplicationFactor = rf
		}
	})()

	go cluster.RecoverNodes(hintsStorage, recoveryStorage, nodeCollection)

	httpConfig := service.Config{
		BindAddr: *bindClientAddr,
		BindPort: *bindClientPort,
	}

	// Starting the service here so that the node can receive writes while joining.
	// TODO Create a cluster manager component that uses all these storage components etc to not
	// have to pass all of them along.
	go service.Start(resolver, partitioner, recoveryStorage, partitionKeyStorage, nodeStorage, httpConfig)

	if isNew {
		mtx, err := tokenStorage.Lock()
		handleErr(err)
		isFirstNode, err := tokenStorage.InitMany(localNode.Name, 16)
		if err != nil {
			log.Println("Intitation of tokens failed")
			handleErr(err)
		}
		if !isFirstNode {
			log.Println("Joining existing cluster")
			// Setting the status to recovering will prevent writes.
			localNode.Status = cluster.NodeStatusRecovering
			nodeStorage.Save(localNode)

			err = join(localNode, tokenStorage, resolver)
			handleErr(err)

			localNode.Status = cluster.NodeStatusUp
			err = nodeStorage.Save(localNode)
			// If this fails, the node will be stuck in the wrong state unable to receive writes
			handleErr(err)
		}
		mtx.Unlock(context.Background())
	} else {
		// TODO check if importing data from initial sync or from a node being deleted, if so, then resume.
	}

	// Sleep forever
	select {}
}

func tokensToString(tokens []int, sep string) string {
	res := make([]string, len(tokens))
	for i, token := range tokens {
		res[i] = strconv.Itoa(token)
	}
	return strings.Join(res, sep)
}

func join(localNode *cluster.Node, tokenStorage *cluster.EtcdTokenStorage, resolver *cluster.Resolver) error {
	toSteal, err := tokenStorage.SuggestReservations()
	log.Printf("Stealing %d tokens: [%s]", len(toSteal), tokensToString(toSteal, " "))
	if err != nil {
		return err
	}
	handleErr(err)
	reserved := []int{}
	for _, tokenToSteal := range toSteal {

		ok, err := tokenStorage.Reserve(tokenToSteal, localNode.Name)
		if err != nil {
			return err
		}
		if ok {
			reserved = append(reserved, tokenToSteal)
		}
		// TODO handle not ok
	}

	log.Println("Starting import of primary data")
	importer := &syncing.BasicImporter{}
	importer.Import(reserved, resolver, localNode.DataLocation)

	oldPrimaries := map[int]*cluster.Node{}
	for _, token := range reserved {
		oldPrimaries[token] = resolver.FindPrimary(token)
		tokenStorage.Release(token)
		tokenStorage.Assign(token, localNode.Name)

		// Update the resolver with the most current assignments.
		resolver.AddToken(token, localNode)
	}

	// This takes one token and finds what tokens are also replicated to to the same node this node is assigned to.
	// This can only be done after assigning the tokens as the resolver needs to understand which
	// nodes tokens are allocated to, as the logic is skipping tokens assigned to the same node.
	secondaryTokens := []int{}
	for _, token := range reserved {
		secondaryTokens = append(secondaryTokens, resolver.ReverseSecondaryLookup(token)...)
	}
	if len(secondaryTokens) > 0 {
		log.Println("Starting import of replicated data")
		importer.Import(secondaryTokens, resolver, localNode.DataLocation)
	}

	// The filtered list of primaries which not longer should hold data for assigned tokens.
	deleteMap := map[int]*cluster.Node{}
	for token, node := range oldPrimaries {
		// check if the token still resolves the location
		// if not, the data should be deleted
		shouldDelete := true
		for _, replLoc := range resolver.FindByKey(token, cluster.WRITE) {
			if replLoc == node.DataLocation {
				shouldDelete = false
			}
		}
		if shouldDelete {
			deleteMap[token] = node
		}
	}
	deleteTokensData(deleteMap)
	return nil
}

func deleteTokensData(tokenLocations map[int]*cluster.Node) {
	/*
		this should just add a job in a queue to be picked up by the agent running at that node.
		that is if we want to be able to add a new node while another one is unavailable.
		if this is not a requirement, we can just make the delete request here.
		The danger with having the same data on multiple locations without intended replication,
		queries merging data from multiple nodes may receive incorrect results.
		This could however be avoided by filtering on partitionToken for those that should be on that
		node. An alternative is to have a background job that clears out data from nodes where it should not be.
	*/
	g := sync.WaitGroup{}
	g.Add(len(tokenLocations))
	for token, node := range tokenLocations {
		go (func() {
			syncing.Delete(token, node.DataLocation)
			g.Done()
		})()
	}
	g.Wait()
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

func handleErr(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func startImporter(etcdClient *clientv3.Client, resolver *cluster.Resolver, localNode cluster.Node) (*syncing.ReliableImporter, cluster.WorkQueue) {
	importer := &syncing.BasicImporter{}
	wq := cluster.NewEtcdWorkQueue(etcdClient, localNode.Name, syncing.ReliableImportWorkName)
	reliableImporter := syncing.NewReliableImporter(importer, wq, resolver, localNode.DataLocation)
	go reliableImporter.Start()
	return reliableImporter, wq
}
