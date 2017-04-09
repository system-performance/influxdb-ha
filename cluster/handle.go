package cluster

import (
	"github.com/hashicorp/memberlist"
	"log"
)

type TokenDelegate interface {
	NotifyNewToken(token int, node *Node)
	NotifyRemovedToken(token int, node *Node)
}

type Config struct {
	BindAddr     string
	BindPort     int
	MetaFilename string
}

func (c Config) SetDefaults() {
	if c.BindPort == 0 {
		c.BindPort = 18086
	}
	if c.MetaFilename == "" {
		c.MetaFilename = "/var/opt/influxdb-ha/meta"
	}
}

type Handle struct {
	list          *memberlist.Memberlist
	Nodes         map[string]*Node
	TokenDelegate *TokenDelegate
	LocalNode     *LocalNode
	Config        Config
}

func NewHandle(config Config) (*Handle, error) {
	handle := &Handle{}
	config.SetDefaults()
	handle.Config = config
	handle.Nodes = make(map[string]*Node)
	nodeErr := handle.createLocalNode(config)
	if nodeErr != nil {
		return handle, nodeErr
	}

	conf := memberlist.DefaultWANConfig()
	conf.Events = eventDelegate{handle}
	conf.BindAddr = config.BindAddr
	if config.BindPort != 0 {
		conf.BindPort = config.BindPort
	} else {
		conf.BindPort = 18086
	}
	log.Printf("[Cluster] Listening on %s:%d", conf.BindAddr, conf.BindPort)
	list, err := memberlist.Create(conf)
	if err != nil {
		return handle, err
	}
	handle.list = list

	return handle, nil
}

// Join connects to one or more seed nodes to join the cluster.
func (h *Handle) Join(existing []string) error {
	_, err := h.list.Join(existing)
	if err != nil {
		return err
	}
	for _, member := range h.list.Members() {
		h.addMember(member)
	}
	return nil
}

func (h *Handle) createLocalNode(config Config) error {
	filePath := config.MetaFilename
	if filePath == "" {
		filePath = "/var/opt/influxdb-ha/meta"
	}
	storage, err := openBoltStorage(filePath)
	if err != nil {
		return err
	}
	h.LocalNode = CreateNodeWithStorage(storage)
	return h.LocalNode.Init()
}

func (h *Handle) RemoveNode(name string) {
	panic("Not implemented")
}

func (h *Handle) addMember(member *memberlist.Node) {
	if _, ok := h.Nodes[member.Name]; !ok {
		node := &Node{}
		node.updateFromBytes(member.Meta)
		node.Name = member.Name
		// the resolver needs to be aware of new tokens.
		h.Nodes[member.Name] = node
		log.Printf("[Cluster] Added cluster member %s", member.Name)
		if h.TokenDelegate != nil {
			for _, token := range node.Tokens {
				(*h.TokenDelegate).NotifyNewToken(token, node)
			}
		}
	}
}

type eventDelegate struct {
	handle *Handle
}

func (e eventDelegate) NotifyJoin(member *memberlist.Node) {
	e.handle.addMember(member)
}

func (e eventDelegate) NotifyLeave(member *memberlist.Node) {
	if node, ok := e.handle.Nodes[member.Name]; ok {
		node.Status = STATUS_REMOVED
		log.Printf("[Cluster] Member removed %s", member.Name)
		// TODO Don't remove tokens until specifically told so. Listen for a broad-casted remove message.
		delete(e.handle.Nodes, member.Name)
		if e.handle.TokenDelegate != nil {
			for _, token := range node.Tokens {
				(*e.handle.TokenDelegate).NotifyRemovedToken(token, node)
			}
		}
	}
}

func (e eventDelegate) NotifyUpdate(member *memberlist.Node) {
	if node, ok := e.handle.Nodes[member.Name]; ok {
		oldTokens := []int{}
		copy(oldTokens, node.Tokens)
		node.updateFromBytes(member.Meta)
		newTokens := node.Tokens
		removed, added := compareIntSlices(oldTokens, newTokens)
		if e.handle.TokenDelegate != nil {
			for _, token := range removed {
				(*e.handle.TokenDelegate).NotifyRemovedToken(token, node)
			}
			for _, token := range added {
				(*e.handle.TokenDelegate).NotifyNewToken(token, node)
			}
		}
	}
}

func compareIntSlices(a []int, b []int) ([]int, []int) {
	amap := map[int]bool{}
	for _, token := range a {
		amap[token] = true
	}
	bmap := map[int]bool{}
	for _, token := range b {
		bmap[token] = true
	}
	adiff := []int{}
	for _, token := range a {
		_, ok := bmap[token]
		if !ok {
			adiff = append(adiff, token)
		}
	}
	bdiff := []int{}
	for _, token := range b {
		_, ok := amap[token]
		if !ok {
			bdiff = append(bdiff, token)
		}
	}
	return adiff, bdiff
}
