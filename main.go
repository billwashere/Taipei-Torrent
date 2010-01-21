package main

import (
	"bytes"
	"crypto/sha1"
	"flag"
	"fmt"
	"jackpal/http"
	"io"
	"log"
	"net"
	"os"
	"rand"
	"strconv"
	"time"
)

var torrent *string = flag.String("torrent", "", "URL or path to a torrent file")
var fileDir *string = flag.String("fileDir", "", "path to directory where files are stored")
var debugp *bool = flag.Bool("debug", false, "Turn on debugging")
var port *int = flag.Int("port", 0, "Port to listen on. Defaults to random.")
var useUPnP *bool = flag.Bool("useUPnP", false, "Use UPnP to open port in firewall.")

const NS_PER_S = 1000000000

func peerId() string {
	sid := "Taipei_tor_" + strconv.Itoa(os.Getpid()) + "______________"
	return sid[0:20]
}

func binaryToDottedPort(port string) string {
	return fmt.Sprintf("%d.%d.%d.%d:%d", port[0], port[1], port[2], port[3],
		(uint16(port[4])<<8)|uint16(port[5]))
}

func chooseListenPort() (listenPort int, err os.Error) {
	listenPort = *port
	if *useUPnP {
		// TODO: Look for ports currently in use. Handle collisions.
		var nat NAT
		nat, err = Discover()
		if err != nil {
			log.Stderr("Unable to discover NAT")
			return
		}
		err = nat.ForwardPort("TCP", listenPort, listenPort, "Taipei-Torrent", 0)
		if err != nil {
			log.Stderr("Unable to forward listen port")
			return
		}
	}
	return
}

func listenForPeerConnections(listenPort int, conChan chan net.Conn) {
	listenString := ":" + strconv.Itoa(listenPort)
	log.Stderr("Listening for peers on port:", listenString)
	listener, err := net.Listen("tcp", listenString)
	if err != nil {
		log.Stderr("Listen failed:", err)
		return
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Stderr("Listener failed:", err)
		} else {
			conChan <- conn
		}
	}
}

var kBitTorrentHeader = []byte{'\x13', 'B', 'i', 't', 'T', 'o', 'r',
	'r', 'e', 'n', 't', ' ', 'p', 'r', 'o', 't', 'o', 'c', 'o', 'l'}

func string2Bytes(s string) []byte { return bytes.NewBufferString(s).Bytes() }

type ActivePiece struct {
	downloaderCount []int // -1 means piece is already downloaded
	pieceLength int
}

func (a *ActivePiece) chooseBlockToDownload(endgame bool) (index int) {
	if endgame {
	    return a.chooseBlockToDownloadEndgame()
	}
	return a.chooseBlockToDownloadNormal()
}

func (a *ActivePiece) chooseBlockToDownloadNormal() (index int) {
	for i, v := range (a.downloaderCount) {
		if v == 0 {
			a.downloaderCount[i]++
			return i
		}
	}
	return -1
}

func (a *ActivePiece) chooseBlockToDownloadEndgame() (index int) {
	index, minCount := -1, -1
	for i, v := range (a.downloaderCount) {
		if v >= 0 && (minCount == -1 || minCount > v)  {
			index, minCount = i, v
		}
	}
	if index > -1 {
	    a.downloaderCount[index]++
	}
	return
}

func (a *ActivePiece) recordBlock(index int) (requestCount int) {
    requestCount = a.downloaderCount[index]
    a.downloaderCount[index] = -1
    return
}

func (a *ActivePiece) isComplete() bool {
	for _, v := range (a.downloaderCount) {
		if v != -1 {
			return false
		}
	}
	return true
}

type TorrentSession struct {
	m               *MetaInfo
	si              *SessionInfo
	ti              *TrackerResponse
	fileStore       FileStore
	peers           map[string]*peerState
	peerMessageChan chan peerMessage
	pieceSet        *Bitset // The pieces we have
	totalPieces     int
	totalSize       int64
	lastPieceLength int
	goodPieces      int
	activePieces    map[int]*ActivePiece
}

func NewTorrentSession(torrent string) (ts *TorrentSession, err os.Error) {
	t := &TorrentSession{peers: make(map[string]*peerState),
		peerMessageChan: make(chan peerMessage),
		activePieces: make(map[int]*ActivePiece)}
	t.m, err = getMetaInfo(torrent)
	if err != nil {
		return
	}
	log.Stderr("Tracker:", t.m.Announce, "Comment:", t.m.Comment, "Encoding:", t.m.Encoding)

	fileStore, totalSize, err := NewFileStore(&t.m.Info, *fileDir)
	if err != nil {
		return
	}
	t.fileStore = fileStore
	t.totalSize = totalSize
	t.lastPieceLength = int(t.totalSize % t.m.Info.PieceLength)

	log.Stderr("Computing pieces left")
	good, bad, pieceSet, err := checkPieces(t.fileStore, totalSize, t.m)
	t.pieceSet = pieceSet
	t.totalPieces = int(good + bad)
	t.goodPieces = int(good)
	log.Stderr("Good pieces:", good, "Bad pieces:", bad)

	listenPort, err := chooseListenPort()
	if err != nil {
		log.Stderr("Could not choose listen port.")
		return
	}
	left := bad * t.m.Info.PieceLength
	if ! t.pieceSet.IsSet(t.totalPieces-1) {
	    left = left - t.m.Info.PieceLength + int64(t.lastPieceLength)
	}
	t.si = &SessionInfo{PeerId: peerId(), Port: listenPort, Left: left}
	return t, err
}

func (t *TorrentSession) fetchTrackerInfo(ch chan *TrackerResponse) {
	m, si := t.m, t.si
	log.Stderr("Stats: Uploaded", si.Uploaded, "Downloaded", si.Downloaded, "Left", si.Left)
	url := m.Announce + "?" +
		"info_hash=" + http.URLEscape(m.InfoHash) +
		"&peer_id=" + si.PeerId +
		"&port=" + strconv.Itoa(si.Port) +
		"&uploaded=" + strconv.Itoa64(si.Uploaded) +
		"&downloaded=" + strconv.Itoa64(si.Downloaded) +
		"&left=" + strconv.Itoa64(si.Left) +
		"&compact=1"
	if t.ti == nil {
		url += "&event=started"
	}
	go func() {
		ti, err := getTrackerInfo(url)
		if err != nil {
			log.Stderr("Could not fetch tracker info:", err)
		} else {
			ch <- ti
		}
	}()
}

func connectToPeer(peer string, ch chan net.Conn) {
	// log.Stderr("Connecting to", peer)
	conn, err := net.Dial("tcp", "", peer)
	if err != nil {
		// log.Stderr("Failed to connect to", peer, err)
	} else {
	    // log.Stderr("Connected to", peer)
		ch <- conn
	}
}

func (t *TorrentSession) AddPeer(conn net.Conn) {
	peer := conn.RemoteAddr().String()
	// log.Stderr("Adding peer", peer)
	ps := NewPeerState(conn)
	ps.address = peer
	var header [68]byte
	copy(header[0:], kBitTorrentHeader[0:])
	copy(header[28:48], string2Bytes(t.m.InfoHash))
	copy(header[48:68], string2Bytes(t.si.PeerId))

	t.peers[peer] = ps
	go peerWriter(ps.conn, ps.writeChan, header[0:])
	go peerReader(ps.conn, ps, t.peerMessageChan)
	ps.SetChoke(false) // TODO: better choke policy
}

func (t *TorrentSession) ClosePeer(peer *peerState) {
    // log.Stderr("Closing peer", peer.address)
    _ = t.removeRequests(peer)
	peer.Close()
	t.peers[peer.address] = peer, false
}

func doTorrent() (err os.Error) {
	log.Stderr("Fetching torrent.")
	ts, err := NewTorrentSession(*torrent)
	if err != nil {
		return
	}
	rechokeChan := time.Tick(10 * NS_PER_S)
	// Start out polling tracker every 20 seconds untill we get a response.
	// Maybe be exponential backoff here?
	retrackerChan := time.Tick(20 * NS_PER_S)
	keepAliveChan := time.Tick(60 * NS_PER_S)
	trackerInfoChan := make(chan *TrackerResponse)

	conChan := make(chan net.Conn)

	go listenForPeerConnections(ts.si.Port, conChan)

	ts.fetchTrackerInfo(trackerInfoChan)

	for {
		select {
		case _ = <-retrackerChan:
			ts.fetchTrackerInfo(trackerInfoChan)
		case ti := <-trackerInfoChan:
			ts.ti = ti
			log.Stderr("Torrent has", ts.ti.Complete, "seeders and", ts.ti.Incomplete, "leachers.")
			peers := ts.ti.Peers
			for i := 0; i < len(peers); i += 6 {
				peer := binaryToDottedPort(peers[i : i+6])
				if _, ok := ts.peers[peer]; !ok {
					go connectToPeer(peer, conChan)
				}
			}
			interval := ts.ti.Interval
			if interval < 120 {
				interval = 120
			} else if interval > 24*3600 {
				interval = 24 * 3600
			}
			log.Stderr("..checking again in", interval, "seconds.")
			retrackerChan = time.Tick(int64(interval) * NS_PER_S)

		case pm := <-ts.peerMessageChan:
			peer, message := pm.peer, pm.message
			peer.lastReadTime = time.Seconds()
			err2 := ts.DoMessage(peer, message)
			if err2 != nil {
				// log.Stderr("Closing peer", peer.address, "because", err2)
				ts.ClosePeer(peer)
				// TODO consider looking for more peers
			}
		case conn := <-conChan:
			ts.AddPeer(conn)
		case _ = <-rechokeChan:
			// TODO: recalculate who to choke / unchoke
			log.Stderr("Peers:", len(ts.peers), "downloaded:", ts.si.Downloaded)
		case _ = <-keepAliveChan:
			now := time.Seconds()
			for _, peer := range (ts.peers) {
			    if peer.lastReadTime != 0 && now - peer.lastReadTime > 3 * 60 {
			        // log.Stderr("Closing peer", peer.address, "because timed out.")
			        ts.ClosePeer(peer)
			        continue
			    }
				err2 := ts.doCheckRequests(peer)
				if err2 != nil {
					if err2 != os.EOF {
						// log.Stderr("Closing peer", peer.address, "because", err2)
					}
					ts.ClosePeer(peer)
					continue
				}
				peer.keepAlive(now)
			}
		}
	}
	return
}

func (t *TorrentSession) RequestBlock(p *peerState) (err os.Error) {
	for k, _ := range (t.activePieces) {
		if p.have.IsSet(k) {
			err = t.RequestBlock2(p, k, false)
			if err != os.EOF {
				return
			}
		}
	}
	// No active pieces. (Or no suitable active pieces.) Pick one
	piece := t.ChoosePiece(p)
	if piece < 0 {
	    // No unclaimed pieces. See if we can double-up on an active piece
	    for k, _ := range (t.activePieces) {
			if p.have.IsSet(k) {
				err = t.RequestBlock2(p, k, true)
				if err != os.EOF {
					return
				}
			}
	    }
	}
	if piece >= 0 {
	    pieceLength := int(t.m.Info.PieceLength)
	    if piece == t.totalPieces - 1 {
	        pieceLength = t.lastPieceLength
	    }
		pieceCount := (pieceLength + STANDARD_BLOCK_LENGTH - 1) / STANDARD_BLOCK_LENGTH
		t.activePieces[piece] = &ActivePiece{make([]int, pieceCount), pieceLength}
		return t.RequestBlock2(p, piece, false)
	} else {
		p.SetInterested(false)
	}
	return
}

func (t *TorrentSession) ChoosePiece(p *peerState) (piece int) {
	n := t.totalPieces
	start := rand.Intn(n)
	piece = t.checkRange(p, start, n)
	if piece == -1 {
		piece = t.checkRange(p, 0, start)
	}
	return
}

func (t *TorrentSession) checkRange(p *peerState, start, end int) (piece int) {
	for i := start; i < end; i++ {
		if (!t.pieceSet.IsSet(i)) && p.have.IsSet(i) {
			if _, ok := t.activePieces[i]; !ok {
				return i
			}
		}
	}
	return -1
}

func (t *TorrentSession) RequestBlock2(p *peerState, piece int, endGame bool) (err os.Error) {
	v := t.activePieces[piece]
	block := v.chooseBlockToDownload(endGame)
	if block >= 0 {
		t.requestBlockImp(p, piece, block, true)
	} else {
		return os.EOF
	}
	return
}

// Used to request or cancel a block
func (t *TorrentSession) requestBlockImp(p *peerState, piece int, block int, request bool) {
	begin := block * STANDARD_BLOCK_LENGTH
	req := make([]byte, 13)
	opcode := byte(6)
	if !request {
	    opcode = byte(8) // Cancel
	}
	length := STANDARD_BLOCK_LENGTH
	if piece == t.totalPieces-1 {
	    left := t.lastPieceLength - begin
	    if left < length {
	        length = left
	    }
	}
	// log.Stderr("Requesting block", piece, ".", block, length, request)
	req[0] = opcode
	uint32ToBytes(req[1:5], uint32(piece))
	uint32ToBytes(req[5:9], uint32(begin))
	uint32ToBytes(req[9:13], uint32(length))
	requestIndex :=  (uint64(piece)<<32)|uint64(begin)
	p.our_requests[requestIndex] = time.Seconds(), request
	p.sendMessage(req)
	return
}

func (t *TorrentSession) RecordBlock(p *peerState, piece, begin, length uint32) (err os.Error) {
	block := begin / STANDARD_BLOCK_LENGTH
	// log.Stderr("Received block", piece, ".", block)
	requestIndex := (uint64(piece)<<32)|uint64(begin)
	p.our_requests[requestIndex] = 0, false
	v, ok := t.activePieces[int(piece)]
	if ok {
		requestCount := v.recordBlock(int(block))
		if requestCount > 1 {
		    // Someone else has also requested this, so send cancel notices
		    for _, peer := range(t.peers) {
		        if p != peer {
		            if _, ok := peer.our_requests[requestIndex]; ok {
		                t.requestBlockImp(peer, int(piece), int(block), false)
		                requestCount--
		            }
		        }
		    }
		}
		t.si.Downloaded += int64(length)
		if v.isComplete() {
			t.activePieces[int(piece)] = v, false
			// TODO: Check if the hash for this piece is good or not.
			t.si.Left -= int64(v.pieceLength)
			t.pieceSet.Set(int(piece))
			t.goodPieces++
			log.Stderr("Have", t.goodPieces, "of", t.totalPieces, "blocks.")
			for _, p := range (t.peers) {
				if p.have != nil {
					if p.have.IsSet(int(piece)) {
						// We don't do anything special. We rely on the caller
						// to decide if this peer is still interesting.
					} else {
						// log.Stderr("...telling ", p)
						haveMsg := make([]byte, 5)
						haveMsg[0] = 4
						uint32ToBytes(haveMsg[1:5], uint32(piece))
						p.sendMessage(haveMsg)
					}
				}
			}
		}
	} else {
		log.Stderr("Duplicate.")
	}
	return
}

func (t *TorrentSession) doChoke(p *peerState) (err os.Error) {
	p.peer_choking = true
	err = t.removeRequests(p)
	return
}

func (t *TorrentSession) removeRequests(p *peerState) (err os.Error) {
	for k, _ := range (p.our_requests) {
		piece := int(k >> 32)
		begin := int(k)
		block := begin / STANDARD_BLOCK_LENGTH
		// log.Stderr("Forgetting we requested block ", piece, ".", block)
		t.removeRequest(piece, block)
	}
	p.our_requests = make(map[uint64]int64, MAX_OUR_REQUESTS)
	return
}

func (t *TorrentSession) removeRequest(piece, block int) {
    v, ok := t.activePieces[piece]
	if ok && v.downloaderCount[block] > 0 {
		v.downloaderCount[block]--
	}
}

func (t *TorrentSession) doCheckRequests(p *peerState) (err os.Error) {
	now := time.Seconds()
	for k, v := range (p.our_requests) {
	    if now - v > 30 {
			piece := int(k >> 32)
			block := int(k)/ STANDARD_BLOCK_LENGTH
			log.Stderr("timing out request of", piece, ".", block)
			t.removeRequest(piece, block)
		}
	}
	return
}

func (t *TorrentSession) DoMessage(p *peerState, message []byte) (err os.Error) {
	if len(p.id) == 0 {
		// This is the header message from the peer.
		if message == nil {
			return os.NewError("missing header")
		}
		peersInfoHash := string(message[8:28])
		if peersInfoHash != t.m.InfoHash {
			return os.NewError("this peer doesn't have the right info hash")
		}
		p.id = string(message[28:48])
	} else {
		if len(message) == 0 { // keep alive
			return
		}
		messageId := message[0]
		// Message 5 is optional, but must be sent as the first message.
		if p.have == nil && messageId != 5 {
			// Fill out the have bitfield
			p.have = NewBitset(t.totalPieces)
		}
		switch id := message[0]; id {
		case 0:
			// log.Stderr("choke", p.address)
			if len(message) != 1 {
				return os.NewError("Unexpected length")
			}
			err = t.doChoke(p)
		case 1:
			// log.Stderr("unchoke", p.address)
			if len(message) != 1 {
				return os.NewError("Unexpected length")
			}
			p.peer_choking = false
			for i := 0; i < MAX_OUR_REQUESTS; i++ {
				err = t.RequestBlock(p)
				if err != nil {
					return
				}
			}
		case 2:
			// log.Stderr("interested", p)
			if len(message) != 1 {
				return os.NewError("Unexpected length")
			}
			p.peer_interested = true
			// TODO: Consider unchoking
		case 3:
			// log.Stderr("not interested", p)
			if len(message) != 1 {
				return os.NewError("Unexpected length")
			}
			p.peer_interested = false
		case 4:
			if len(message) != 5 {
				return os.NewError("Unexpected length")
			}
			n := bytesToUint32(message[1:])
			if n < uint32(p.have.n) {
				p.have.Set(int(n))
				if !p.am_interested && !t.pieceSet.IsSet(int(n)) {
					p.SetInterested(true)
				}
			} else {
				return os.NewError("have index is out of range.")
			}
		case 5:
			// log.Stderr("bitfield", p.address)
			if p.have != nil {
				return os.NewError("Late bitfield operation")
			}
			p.have = NewBitsetFromBytes(t.totalPieces, message[1:])
			if p.have == nil {
				return os.NewError("Invalid bitfield data.")
			}
			t.checkInteresting(p)
		case 6:
			// log.Stderr("request")
			if len(message) != 13 {
				return os.NewError("Unexpected message length")
			}
			index := bytesToUint32(message[1:5])
			begin := bytesToUint32(message[5:9])
			length := bytesToUint32(message[9:13])
			if index >= uint32(p.have.n) {
				return os.NewError("piece out of range.")
			}
			if !t.pieceSet.IsSet(int(index)) {
				return os.NewError("we don't have that piece.")
			}
			if int64(begin) >= t.m.Info.PieceLength {
				return os.NewError("begin out of range.")
			}
			if int64(begin)+int64(length) > t.m.Info.PieceLength {
				return os.NewError("begin + length out of range.")
			}
			if length != STANDARD_BLOCK_LENGTH {
				return os.NewError("Unexpected block length.")
			}
			// TODO: Asynchronous
			// p.AddRequest(index, begin, length)
			return t.sendRequest(p, index, begin, length)
		case 7:
			// piece
			if len(message) < 9 {
				return os.NewError("unexpected message length")
			}
			index := bytesToUint32(message[1:5])
			begin := bytesToUint32(message[5:9])
			length := len(message) - 9
			if index >= uint32(p.have.n) {
				return os.NewError("piece out of range.")
			}
			if t.pieceSet.IsSet(int(index)) {
				// We already have that piece, keep going
				break
			}
			if int64(begin) >= t.m.Info.PieceLength {
				return os.NewError("begin out of range.")
			}
			if int64(begin)+int64(length) > t.m.Info.PieceLength {
				return os.NewError("begin + length out of range.")
			}
			if length > 128*1024 {
				return os.NewError("Block length too large.")
			}
			globalOffset := int64(index)*t.m.Info.PieceLength + int64(begin)
			_, err = t.fileStore.WriteAt(message[9:], globalOffset)
			if err != nil {
				return err
			}
			t.RecordBlock(p, index, begin, uint32(length))
			err = t.RequestBlock(p)
		case 8:
			log.Stderr("cancel")
			if len(message) != 13 {
				return os.NewError("Unexpected message length")
			}
			index := bytesToUint32(message[1:5])
			begin := bytesToUint32(message[5:9])
			length := bytesToUint32(message[9:13])
			if index >= uint32(p.have.n) {
				return os.NewError("piece out of range.")
			}
			if !t.pieceSet.IsSet(int(index)) {
				return os.NewError("we don't have that piece.")
			}
			if int64(begin) >= t.m.Info.PieceLength {
				return os.NewError("begin out of range.")
			}
			if int64(begin)+int64(length) > t.m.Info.PieceLength {
				return os.NewError("begin + length out of range.")
			}
			if length != STANDARD_BLOCK_LENGTH {
				return os.NewError("Unexpected block length.")
			}
			p.CancelRequest(index, begin, length)
		case 9:
			// TODO: Implement this message.
			// We see peers sending us 16K byte messages here, so
			// it seems that we don't understand what this is.
			log.Stderr("port len=", len(message))
			//if len(message) != 3 {
			//	return os.NewError("Unexpected length")
			//}
		default:
			return os.NewError("Uknown message id")
		}
	}
	return
}

func (t *TorrentSession) sendRequest(peer *peerState, index, begin, length uint32) (err os.Error) {
	if !peer.am_choking {
		// log.Stderr("Sending block", index, begin)
		buf := make([]byte, length+9)
		buf[0] = 7
		uint32ToBytes(buf[1:5], index)
		uint32ToBytes(buf[5:9], begin)
		_, err = t.fileStore.ReadAt(buf[9:],
			int64(index)*t.m.Info.PieceLength+int64(begin))
		if err != nil {
			return
		}
		peer.sendMessage(buf)
		t.si.Uploaded += STANDARD_BLOCK_LENGTH
	}
	return
}

func (t *TorrentSession) checkInteresting(p *peerState) {
	p.SetInterested(t.isInteresting(p))
}

func (t *TorrentSession) isInteresting(p *peerState) bool {
	for i := 0; i < t.totalPieces; i++ {
		if !t.pieceSet.IsSet(i) && p.have.IsSet(i) {
			return true
		}
	}
	return false
}

const MAX_OUR_REQUESTS = 2
const MAX_PEER_REQUESTS = 10
const STANDARD_BLOCK_LENGTH = 16 * 1024

type peerState struct {
	address         string
	id              string
	writeChan       chan []byte
	lastWriteTime   int64   // In seconds
	lastReadTime   int64   // In seconds
	have            *Bitset // What the peer has told us it has
	conn            net.Conn
	am_choking      bool // this client is choking the peer
	am_interested   bool // this client is interested in the peer
	peer_choking    bool // peer is choking this client
	peer_interested bool // peer is interested in this client
	peer_requests   map[uint64]bool
	our_requests    map[uint64]int64 // What we requested, when we requested it
}

func NewPeerState(conn net.Conn) *peerState {
	writeChan := make(chan []byte)
	return &peerState{writeChan: writeChan, conn: conn,
		am_choking: true, peer_choking: true,
		peer_requests: make(map[uint64]bool, MAX_PEER_REQUESTS),
		our_requests: make(map[uint64]int64, MAX_OUR_REQUESTS)}
}

func (p *peerState) Close() {
	p.conn.Close()
	close(p.writeChan)
}

func (p *peerState) AddRequest(index, begin, length uint32) {
	if !p.am_choking && len(p.peer_requests) < MAX_PEER_REQUESTS {
		offset := (uint64(index) << 32) | uint64(begin)
		p.peer_requests[offset] = true
	}
}

func (p *peerState) CancelRequest(index, begin, length uint32) {
	offset := (uint64(index) << 32) | uint64(begin)
	if _, ok := p.peer_requests[offset]; ok {
		p.peer_requests[offset] = false, false
	}
}

func (p *peerState) RemoveRequest() (index, begin, length uint32, ok bool) {
	for k, _ := range (p.peer_requests) {
		index, begin = uint32(k>>32), uint32(k)
		length = STANDARD_BLOCK_LENGTH
		ok = true
		return
	}
	return
}

func (p *peerState) SetChoke(choke bool) {
	if choke != p.am_choking {
		p.am_choking = choke
		b := byte(1)
		if choke {
			b = 0
			p.peer_requests = make(map[uint64]bool, MAX_PEER_REQUESTS)
		}
		p.sendOneCharMessage(b)
	}
}

func (p *peerState) SetInterested(interested bool) {
	if interested != p.am_interested {
	    // log.Stderr("SetInterested", interested, p.address)
		p.am_interested = interested
		b := byte(3)
		if interested {
			b = 2
		}
		p.sendOneCharMessage(b)
	}
}

func (p *peerState) sendOneCharMessage(b byte) {
    // log.Stderr("ocm", b, p.address)
	p.sendMessage([]byte{b})
}

func (p *peerState) sendMessage(b []byte) {
	p.writeChan <- b
	p.lastWriteTime = time.Seconds()
}

func (p *peerState) keepAlive(now int64) {
	if now-p.lastWriteTime >= 120 {
		// log.Stderr("Sending keep alive", p)
		p.sendMessage([]byte{})
	}
}

// There's two goroutines per peer, one to read data from the peer, the other to
// send data to the peer.

func uint32ToBytes(buf []byte, n uint32) {
	buf[0] = byte(n >> 24)
	buf[1] = byte(n >> 16)
	buf[2] = byte(n >> 8)
	buf[3] = byte(n)
}

func writeNBOUint32(conn net.Conn, n uint32) (err os.Error) {
	var buf [4]byte
	uint32ToBytes(&buf, n)
	_, err = conn.Write(buf[0:])
	return
}

func bytesToUint32(buf []byte) uint32 {
	return (uint32(buf[0]) << 24) |
		(uint32(buf[1]) << 16) |
		(uint32(buf[2]) << 8) | uint32(buf[3])
}

func readNBOUint32(conn net.Conn) (n uint32, err os.Error) {
	var buf [4]byte
	_, err = conn.Read(buf[0:])
	if err != nil {
		return
	}
	n = bytesToUint32(buf[0:])
	return
}

func peerWriter(conn net.Conn, msgChan chan []byte, header []byte) {
	// log.Stderr("Writing header.")
	_, err := conn.Write(header)
	if err != nil {
		return
	}
	// log.Stderr("Writing messages")
	for {
		select {
		case msg := <-msgChan:
			// log.Stderr("Writing", len(msg), conn.RemoteAddr())
			err = writeNBOUint32(conn, uint32(len(msg)))
			if err != nil {
				return
			}
			_, err = conn.Write(msg)
			if err != nil {
				log.Stderr("Failed to write a message", conn, len(msg), msg, err)
				return
			}
		}
	}
	// log.Stderr("peerWriter exiting")
}

type peerMessage struct {
	peer    *peerState
	message []byte // nil when peer is closed
}

func peerReader(conn net.Conn, peer *peerState, msgChan chan peerMessage) {
	// TODO: Add two-minute timeout.
	// log.Stderr("Reading header.")
	var header [68]byte
	_, err := conn.Read(header[0:1])
	if err != nil {
		goto exit
	}
	if header[0] != 19 {
		goto exit
	}
	_, err = conn.Read(header[1:20])
	if err != nil {
		goto exit
	}
	if string(header[1:20]) != "BitTorrent protocol" {
		goto exit
	}
	// Read rest of header
	_, err = conn.Read(header[20:])
	if err != nil {
		goto exit
	}
	msgChan <- peerMessage{peer, header[20:]}
	// log.Stderr("Reading messages")
	for {
		var n uint32
		n, err = readNBOUint32(conn)
		if err != nil {
			goto exit
		}
		if n > 130*1024 {
			// log.Stderr("Message size too large: ", n)
			goto exit
		}
		buf := make([]byte, n)
		_, err := io.ReadFull(conn, buf)
		if err != nil {
			goto exit
		}
		msgChan <- peerMessage{peer, buf}
	}

exit:
	conn.Close()
	msgChan <- peerMessage{peer, nil}
	// log.Stderr("peerWriter exiting")
}


func checkPieces(fs FileStore, totalLength int64, m *MetaInfo) (good, bad int64, goodBits *Bitset, err os.Error) {
	currentSums, err := computeSums(fs, totalLength, m.Info.PieceLength)
	if err != nil {
		return
	}
	pieceLength := m.Info.PieceLength
	numPieces := (totalLength + pieceLength - 1) / pieceLength
	goodBits = NewBitset(int(numPieces))
	ref := m.Info.Pieces
	for i := int64(0); i < numPieces; i++ {
		base := i * sha1.Size
		end := base + sha1.Size
		if checkEqual(ref[base:end], currentSums[base:end]) {
			good++
			goodBits.Set(int(i))
		} else {
			bad++
		}
	}
	return
}

func checkEqual(ref string, current []byte) bool {
	for i := 0; i < len(current); i++ {
		if ref[i] != current[i] {
			return false
		}
	}
	return true
}

func computeSums(fs FileStore, totalLength int64, pieceLength int64) (sums []byte, err os.Error) {
	numPieces := (totalLength + pieceLength - 1) / pieceLength
	sums = make([]byte, sha1.Size*numPieces)
	hasher := sha1.New()
	piece := make([]byte, pieceLength)
	for i := int64(0); i < numPieces; i++ {
	    if i == numPieces - 1 {
	        piece = piece[0:totalLength - i * pieceLength]
	    }
		_, err := fs.ReadAt(piece, i*pieceLength)
		if err != nil {
			return
		}
		hasher.Reset()
		_, err = hasher.Write(piece)
		if err != nil {
			return
		}
		copy(sums[i*sha1.Size:], hasher.Sum())
	}
	return
}

func main() {
	// testBencode()
	// testUPnP()
	flag.Parse()
	log.Stderr("Starting.")
	err := doTorrent()
	if err != nil {
		log.Stderr("Failed: ", err)
	} else {
		log.Stderr("Done")
	}
}
