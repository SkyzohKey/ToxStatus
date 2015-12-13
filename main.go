package main

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const (
	httpListenPort                   = 8081
	refreshRate                      = 60 //in seconds
	wikiURI                          = "https://wiki.tox.chat/users/nodes?do=export_raw"
	maxUDPPacketSize                 = 2048
	getNodesPacketID                 = 2
	sendNodesIpv6PacketID            = 4
	bootstrapInfoPacketID            = 240
	bootstrapInfoPacketLength        = 78
	tcpHandshakePacketLength         = 128
	tcpHandshakeResponsePacketLength = 96
	maxMOTDLength                    = 256
	queryTimeout                     = 4 //in seconds
	dialerTimeout                    = 2 //in seconds
)

var (
	lastScan     int64
	nodesList    = list.New()
	crypto, _    = NewCrypto()
	tcpPorts     = []int{443, 3389, 33445}
	lowerFuncMap = template.FuncMap{"lower": strings.ToLower}
)

type tcpHandshakeResult struct {
	Port  int
	Error error
}

type toxStatus struct {
	LastScan       int64     `json:"last_scan"`
	LastScanString string    `json:"last_scan_string"`
	Nodes          []toxNode `json:"nodes"`
}

type toxNode struct {
	Ipv4Address    string `json:"ipv4"`
	Ipv6Address    string `json:"ipv6"`
	Port           int    `json:"port"`
	TCPPorts       []int  `json:"tcp_ports"`
	PublicKey      string `json:"public_key"`
	Maintainer     string `json:"maintainer"`
	Location       string `json:"location"`
	Status         bool   `json:"status"`
	Version        string `json:"version"`
	MOTD           string `json:"motd"`
	LastPing       int64  `json:"last_ping"`
	LastPingString string `json:"last_ping_string"`
}

func main() {
	if crypto == nil {
		log.Fatalf("Could not generate keypair")
	}

	go probeLoop()

	http.HandleFunc("/", handleHTTPRequest)
	http.HandleFunc("/json", handleJSONRequest)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", httpListenPort), nil))
}

func handleHTTPRequest(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path[1:]
	if r.URL.Path == "/" {
		renderMainPage(w, "index.html")
		return
	}

	//TODO: make this more efficient
	data, err := ioutil.ReadFile(path.Join("./assets/", string(urlPath)))
	if err != nil {
		http.Error(w, http.StatusText(404), 404)
	} else {
		w.Write(data)
	}
}

func renderMainPage(w http.ResponseWriter, urlPath string) {
	tmpl, err := template.New("index.html").
		Funcs(lowerFuncMap).
		ParseFiles(path.Join("./assets/", string(urlPath)))

	if err != nil {
		http.Error(w, http.StatusText(500), 500)
		log.Printf("Internal server error while trying to serve index: %s", err.Error())
	} else {
		nodes := nodesListToSlice(nodesList)
		response := toxStatus{lastScan, time.Unix(lastScan, 0).String(), nodes}
		tmpl.Execute(w, response)
	}
}

func handleJSONRequest(w http.ResponseWriter, r *http.Request) {
	nodes := nodesListToSlice(nodesList)
	response := toxStatus{lastScan, time.Unix(lastScan, 0).String(), nodes}

	bytes, err := json.Marshal(response)
	if err != nil {
		http.Error(w, http.StatusText(500), 500)
		return
	}

	w.Write(bytes)
}

func probeLoop() {
	for {
		nodes, err := parseNodes()
		if err != nil {
			log.Printf("Error while trying to parse nodes: %s", err.Error())
		} else {
			c := make(chan *toxNode)
			for e := nodes.Front(); e != nil; e = e.Next() {
				node, _ := e.Value.(*toxNode)
				go func() { c <- probeNode(node) }()
			}

			for i := 0; i < nodes.Len(); i++ {
				_ = <-c
			}

			nodesList = nodes
			lastScan = time.Now().Unix()
		}

		time.Sleep(refreshRate * time.Second)
	}
}

func probeNode(node *toxNode) *toxNode {
	conn, err := newNodeConn(node, node.Port, "udp")
	if err != nil {
		return node
	}

	err = getBootstrapInfo(node, conn)
	if err != nil {
		fmt.Printf("%s\n", err.Error())
	}

	conn.Close()
	conn, err = newNodeConn(node, node.Port, "udp")
	if err != nil {
		return node
	}

	err = getNodes(node, conn)
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		return node
	}
	conn.Close()

	ports := tcpPorts
	if !contains(tcpPorts, node.Port) {
		ports = append(ports, node.Port)
	}

	c := make(chan tcpHandshakeResult)
	for _, port := range ports {
		go func(p int) {
			conn, err = newNodeConn(node, p, "tcp")
			if err != nil {
				fmt.Printf("%s\n", err.Error())
				c <- tcpHandshakeResult{p, err}
			} else {
				c <- tryTCPHandshake(node, conn, p)
			}
		}(port)
	}

	for i := 0; i < len(ports); i++ {
		result := <-c
		if result.Error != nil {
			fmt.Printf("%s\n", result.Error.Error())
		} else {
			node.TCPPorts = append(node.TCPPorts, result.Port)
		}
	}

	node.LastPing = time.Now().Unix()
	node.Status = true
	return node
}

func getNodes(node *toxNode, conn net.Conn) error {
	nodePublicKey, err := hex.DecodeString(node.PublicKey)
	if err != nil {
		return err
	}

	plain := make([]byte, len(crypto.PublicKey)+8)
	copy(plain, crypto.PublicKey)
	copy(plain[len(crypto.PublicKey):], nextBytes(8)) //ping id

	nonce := nextNonce()
	sharedKey := crypto.CreateSharedKey(nodePublicKey)
	encrypted := encryptData(plain, sharedKey, nonce)[16:]

	payload := make([]byte, 1+len(crypto.PublicKey)+len(nonce)+len(encrypted))
	payload[0] = getNodesPacketID
	copy(payload[1:], crypto.PublicKey)
	copy(payload[1+len(crypto.PublicKey):], nonce)
	copy(payload[1+len(crypto.PublicKey)+len(nonce):], encrypted)
	conn.Write(payload)

	buffer := make([]byte, maxUDPPacketSize)
	_, err = conn.Read(buffer)

	if err != nil {
		return err
	} /*else if payload[0] != sendNodesIpv6PacketID {
		return fmt.Errorf("packet id: %d is not a sendnodesipv6 packet", payload[0])
	}

	right now we're happy if a node responds to our 'getnodes' request, without even validating the response
	this needs some more work

	on a side note: it looks like nodes are sending a 'getnodes' packet before 'sendnodesipv6',
	*/

	return nil
}

func getBootstrapInfo(node *toxNode, conn net.Conn) error {
	payload := make([]byte, bootstrapInfoPacketLength)
	payload[0] = bootstrapInfoPacketID
	conn.Write(payload)

	buffer := make([]byte, 1+4+maxMOTDLength)
	read, err := conn.Read(buffer)

	if err != nil {
		return err
	} else if buffer[0] != bootstrapInfoPacketID {
		return fmt.Errorf("packet id: %d is not a bootstrap info packet", buffer[0])
	}

	buffer = buffer[:read]
	if len(buffer) < 1+4 {
		return errors.New("bootstrap info packet too small")
	}

	node.Version = fmt.Sprintf("%d", binary.BigEndian.Uint32(buffer[1:1+4]))
	node.MOTD = string(bytes.Trim(buffer[1+4:], "\x00"))
	return nil
}

func tryTCPHandshake(node *toxNode, conn net.Conn, port int) tcpHandshakeResult {
	/* NOTE: conn is closed at the end of this function */
	nodePublicKey, err := hex.DecodeString(node.PublicKey)
	if err != nil {
		return tcpHandshakeResult{port, err}
	}

	nonce := nextNonce()
	baseNonce := nextNonce()
	plain := make([]byte, len(crypto.PublicKey)+len(baseNonce))
	tempCrypto, _ := NewCrypto()

	copy(plain, tempCrypto.PublicKey)
	copy(plain[len(tempCrypto.PublicKey):], baseNonce)
	sharedKey := crypto.CreateSharedKey(nodePublicKey)
	encrypted := encryptData(plain, sharedKey, nonce)[16:]

	payload := make([]byte, tcpHandshakePacketLength)
	copy(payload, crypto.PublicKey)
	copy(payload[len(crypto.PublicKey):], nonce)
	copy(payload[len(crypto.PublicKey)+len(nonce):], encrypted)
	conn.Write(payload)

	buffer := make([]byte, tcpHandshakeResponsePacketLength)
	read, err := conn.Read(buffer)

	var result tcpHandshakeResult

	if err != nil {
		result = tcpHandshakeResult{port, err}
	} else if read != tcpHandshakeResponsePacketLength {
		result = tcpHandshakeResult{
			port,
			errors.New("tcp handshake response has an incorrect length"),
		}
	} else {
		result = tcpHandshakeResult{port, nil}
	}

	conn.Close()
	return result
}

func newNodeConn(node *toxNode, port int, network string) (net.Conn, error) {
	dialer := net.Dialer{}
	dialer.Deadline = time.Now().Add(dialerTimeout * time.Second)

	conn, err := dialer.Dial(network, fmt.Sprintf("%s:%d", node.Ipv4Address, port))
	if err != nil {
		return nil, err
	}

	conn.SetReadDeadline(time.Now().Add(queryTimeout * time.Second))
	return conn, nil
}

func parseNode(nodeString string) *toxNode {
	nodeString = stripSpaces(nodeString)
	if !strings.HasPrefix(nodeString, "|") {
		return nil
	}

	lineParts := strings.Split(nodeString, "|")
	if port, err := strconv.Atoi(strings.TrimSpace(lineParts[3])); err == nil && len(lineParts) == 8 {
		node := toxNode{
			strings.TrimSpace(lineParts[1]),
			strings.TrimSpace(lineParts[2]),
			port,
			[]int{},
			strings.TrimSpace(lineParts[4]),
			strings.TrimSpace(lineParts[5]),
			strings.TrimSpace(lineParts[6]),
			false,
			"",
			"",
			0,
			"Never",
		}

		if node.Ipv6Address == "NONE" {
			node.Ipv6Address = "-"
		}

		return &node
	}

	return nil
}

func parseNodes() (*list.List, error) {
	res, err := http.Get(wikiURI)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	nodes := list.New()
	content, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		node := parseNode(line)
		if node == nil {
			continue
		}

		oldNode := getOldNode(node.PublicKey)
		if oldNode != nil { //transfer last ping info
			node.LastPing = oldNode.LastPing
			node.LastPingString = oldNode.LastPingString
		}

		nodes.PushBack(node)
	}
	return nodes, nil
}

func getOldNode(publicKey string) *toxNode {
	for e := nodesList.Front(); e != nil; e = e.Next() {
		node, _ := e.Value.(*toxNode)
		if node.PublicKey == publicKey {
			return node
		}
	}
	return nil
}
