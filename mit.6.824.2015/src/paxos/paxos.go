package paxos

import (
	"encoding/gob"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net"
	"net/rpc"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//
// Paxos library, to be included in an application.
// Multiple applications will run, each including
// a Paxos peer.
//
// Manages a sequence of agreed-on values.
// The set of peers is fixed.
// Copes with network failures (partition, msg loss, &c).
// Does not store anything persistently, so cannot handle crash+restart.
//
// The application interface:
//
// px = paxos.Make(peers []string, me string)
// px.Start(seq int, v interface{}) -- start agreement on new instance
// px.Status(seq int) (Fate, v interface{}) -- get info about an instance
// px.Done(seq int) -- ok to forget all instances <= seq
// px.Max() int -- highest instance seq known, or -1
// px.Min() int -- instances before this seq have been forgotten
//


// px.Status() return values, indicating
// whether an agreement has been decided,
// or Paxos has not yet reached agreement,
// or it was agreed but forgotten (i.e. < Min()).
type Fate int

// use for test
var id int = 1000000

const (
	FATAL  = iota
	ERROR
	INFO
	DEBUG
)

const (
	Decided   Fate = iota + 1
	Pending        // not yet decided.
	Forgotten      // decided but forgotten.
)

/*
the Paxos pseudo-code (for a single instance) from the lecture:

proposer(v):
	while not decided:
	choose n, unique and higher than any n seen so far
	send prepare(n) to all servers including self
	if prepare_ok(n, n_a, v_a) from majority:
		v' = v_a with highest n_a; choose own v otherwise
		send accept(n, v') to all
		if accept_ok(n) from majority:
			send decided(v') to all

acceptor's state:
	n_p (highest prepare seen)
	n_a, v_a (highest accept seen)

acceptor's prepare(n) handler:
	if n > n_p
		n_p = n
		reply prepare_ok(n, n_a, v_a)
	else
		reply prepare_reject

acceptor's accept(n, v) handler:
	if n >= n_p
		n_p = n
		n_a = n
		v_a = v
		reply accept_ok(n)
	else
		reply accept_reject
*/

type InstanceState struct {
	instance                 Instance
	state                    Fate
}

type Instance struct {
	V                        interface{}
	Seq                      int
}

func init() {
	//
	gob.Register(Instance{})
}

type Paxos struct {
	mu         sync.Mutex
	l          net.Listener
	dead       int32 // for testing
	unreliable int32 // for testing
	rpcCount   int32 // for testing
	peers      []string
	me         int // index into peers[]

	// Your data here.
	n_p                         int
	v_p                         interface{}
	n_a                         int
	v_a                         interface{}
	rounding                    bool

	proposeN                    int
	proposeV                    interface{}
	prepareVote                 *PrepareReply
	prepareVoteCounter          int
	prepared                    bool
	accepted                    bool
	decided                     bool
	acceptVoteCounter           int

	instanceStates              []*InstanceState
	instanceIndex               int

	decidedInstances            []*InstanceState

	prepareReplyChan            chan *PrepareExt
	prepareArgsChan             chan *PrepareArgs
	prepareReplyInterChan       chan *PrepareReply
	acceptReplyChan             chan *AcceptExt
	acceptArgsChan              chan *AcceptArgs
	acceptReplyInterChan        chan *AcceptReply
	decidedReplyChan            chan *DecidedExt
	decidedArgsChan             chan *DecidedArgs
	decidedReplyInterChan       chan *DecidedReply
	commandReplyChan            chan *CommandReply
	commandArgsChan             chan *CommandArgs
	exitChan                    chan bool

	timer                       *time.Ticker
	roundTimeout                int

	id                          int
	logLevel                    int
}

func (px *Paxos) dump(prefix string, logLevel int) {
	if logLevel < INFO  {
		return
	}
	dumpLog := fmt.Sprintf(" paxos: %d, %s, paxos state: \n", px.id, prefix)
	dumpLog += fmt.Sprintf("    n_p: %d, n_a: %d, prepare vote counter: %d, accept vote counter: %d, prepared: %v, accepted: %v, decided: %v\n",
		px.n_p, px.n_a, px.prepareVoteCounter, px.acceptVoteCounter, px.prepared, px.accepted, px.decided)
	dumpLog += fmt.Sprintf("    proposed n: %d\n", px.proposeN)
	dumpLog += fmt.Sprintf("    instance index: %d\n", px.instanceIndex)
	dumpLog += "    instance states:"
	for _, item := range px.instanceStates {
		dumpLog += fmt.Sprintf(" [%d,%d] ", item.instance.Seq, item.state)
	}
	dumpLog += "\n"
	dumpLog += "    decided instances:"
	for _, item := range px.decidedInstances {
		dumpLog += fmt.Sprintf(" [%d,%d] ", item.instance.Seq, item.state)
	}
	dumpLog += "\n"
	log.Printf(dumpLog)
}

//
// call() sends an RPC to the rpcname handler on server srv
// with arguments args, waits for the reply, and leaves the
// reply in reply. the reply argument should be a pointer
// to a reply structure.
//
// the return value is true if the server responded, and false
// if call() was not able to contact the server. in particular,
// the replys contents are only valid if call() returned true.
//
// you should assume that call() will time out and return an
// error after a while if it does not get a reply from the server.
//
// please use call() to send all RPCs, in client.go and server.go.
// please do not change this function.
//
func call(srv string, name string, args interface{}, reply interface{}) bool {
	c, err := rpc.Dial("unix", srv)
	if err != nil {
		err1 := err.(*net.OpError)
		if err1.Err != syscall.ENOENT && err1.Err != syscall.ECONNREFUSED {
			fmt.Printf("paxos Dial() failed: %v\n", err1)
		}
		return false
	}
	defer c.Close()

	err = c.Call(name, args, reply)
	if err == nil {
		return true
	}

	fmt.Println(err)
	return false
}

const (
	START   = iota
	DONE
	MAX
	MIN
	STATUS
)

type CommandArgs struct {
	Name    int
	Seq     int
	V       interface{}
}

type CommandReply struct {
	Seq      int
	State    Fate
	V        interface{}
}

func CommandName(name int) string {
	switch name {
	case START:
		return "start"
	case DONE:
		return "done"
	case MAX:
		return "max"
	case MIN:
		return "min"
	case STATUS:
		return "status"
	}
	return ""
}

func (args *CommandArgs) dump(logLevel int, id int) {
	if logLevel < INFO {
		return
	}
	dumpLog := fmt.Sprintf(" paxos: %d, Receive CommandArgs, Name: %s, Seq: %d", id, CommandName(args.Name), args.Seq)
	log.Printf(dumpLog)
}

func (reply *CommandReply) dump(logLevel int, id int) {
	if logLevel < INFO {
		return
	}
	dumpLog := fmt.Sprintf(" paxos: %d, Receive CommandReply, Seq: %d, State: %d", id, reply.Seq, reply.State)
	log.Printf(dumpLog)
}


type PrepareArgs struct {
	N       int
	V       interface{}
}

type PrepareReply struct {
	N        int
	N_a      int
	V_a      interface{}
}

type PrepareExt struct {
	Args      *PrepareArgs
	Reply     *PrepareReply
}

func (args *PrepareArgs) dump(logLevel int, id int) {
	if logLevel < INFO {
		return
	}
	/*
	seq := args.V.(Instance).Seq
	v := args.V.(Instance).V
	dumpLog := fmt.Sprintf(" paxos: %d, Receive PrepareArgs, N: %d, V.Seq: %d, V.V: %v", id, args.N, seq, v)
	*/
	seq := args.V.(Instance).Seq
	dumpLog := fmt.Sprintf(" paxos: %d, Receive PrepareArgs, N: %d, V.Seq: %d", id, args.N, seq)
	log.Printf(dumpLog)
}

func (reply *PrepareReply) dump(logLevel int, id int) {
	if logLevel < INFO{
		return
	}
	/*
	seq := 0
	var v interface{}
	if reply.V_a != nil {
		seq = reply.V_a.(Instance).Seq
		v = reply.V_a.(Instance).V
	}
	dumpLog := fmt.Sprintf(" paxos: %d, Receive PrepareReply, N: %d, N_a: %d, V_a.Seq: %d, V_a.V: %v", id, reply.N, reply.N_a, seq, v)
	*/
	seq := 0
	if reply.V_a != nil {
		seq = reply.V_a.(Instance).Seq
	}
	dumpLog := fmt.Sprintf(" paxos: %d, Receive PrepareReply, N: %d, N_a: %d, V_a.Seq: %d", id, reply.N, reply.N_a, seq)
	log.Printf(dumpLog)
}

func (px *Paxos) Prepare(v interface{}) {
	// choose a n
	n := int(time.Now().Unix())
	n = n << 8
	m := px.id
	m = m & 0xFF
	n = n + m
	px.proposeN = n
	px.proposeV = v
	px.prepareVoteCounter = 0
	px.acceptVoteCounter = 0
	px.prepareVote = nil
	px.prepared = false
	px.accepted = false
	px.decided = false

	args := &PrepareArgs{
		N: px.proposeN,
		V: px.proposeV,
	}
	for _, peer := range px.peers {
		go func(server string) {
			var reply PrepareReply
			call(server, "Paxos.PrepareVote", args, &reply)
			ext := &PrepareExt{
				Args: args,
				Reply: &reply,
			}
			px.prepareReplyChan <- ext
		}(peer)
	}
}

func (px *Paxos) PrepareVote(args *PrepareArgs, reply *PrepareReply) error {
	px.prepareArgsChan <- args
	replyInternal, ok := <- px.prepareReplyInterChan
	if !ok || replyInternal == nil {
		log.Fatal("PrepareVote fatal.")
	} else {
		*reply = *replyInternal
	}
	return nil
}


type AcceptArgs struct {
	N          int
	V          interface{}
}

type AcceptReply struct {
	N          int
}

type AcceptExt struct {
	Args      *AcceptArgs
	Reply     *AcceptReply
}

func (args *AcceptArgs) dump(logLevel int, id int) {
	if logLevel < INFO {
		return
	}
	/*
	seq := args.V.(Instance).Seq
	v := args.V.(Instance).V
	dumpLog := fmt.Sprintf(" paxos: %d, Receive AcceptArgs, N: %d, V.Seq: %d, V.V: %v", id, args.N, seq, v)
	*/
	seq := args.V.(Instance).Seq
	dumpLog := fmt.Sprintf(" paxos: %d, Receive AcceptArgs, N: %d, V.Seq: %d", id, args.N, seq)
	log.Printf(dumpLog)
}

func (reply *AcceptReply) dump(logLevel int, id int) {
	if logLevel < INFO {
		return
	}
	dumpLog := fmt.Sprintf(" paxos: %d, Receive AcceptReply, N: %d", id, reply.N)
	log.Printf(dumpLog)
}

func (px *Paxos) Accept(n int, v interface{}) {
	px.acceptVoteCounter = 0
	args := &AcceptArgs{
		N: n,
		V: v,
	}
	for _, peer := range px.peers {
		go func(server string) {
			var reply AcceptReply
			call(server, "Paxos.AcceptVote", args, &reply)
			ext := &AcceptExt{
				Args: args,
				Reply: &reply,
			}
			px.acceptReplyChan <- ext
		}(peer)
	}
}

func (px *Paxos) AcceptVote(args *AcceptArgs, reply *AcceptReply) error {
	px.acceptArgsChan <- args
	replyInternal, ok := <- px.acceptReplyInterChan
	if !ok || replyInternal == nil {
		log.Fatal("AcceptVote fatal.")
	} else {
		*reply = *replyInternal
	}
	return nil
}

type DecidedArgs struct {
	N          int
	V          interface{}
}

type DecidedReply struct {
	N           int
}

type DecidedExt struct {
	Args      *DecidedArgs
	Reply     *DecidedReply
}

func (args *DecidedArgs) dump(logLevel int, id int) {
	if logLevel < INFO {
		return
	}
	/*
	seq := args.V.(Instance).Seq
	v := args.V.(Instance).V
	dumpLog := fmt.Sprintf(" paxos: %d, Receive DecidedArgs, N: %d, V.Seq: %d, V.V: %v", id, args.N, seq, v)
	*/
	seq := args.V.(Instance).Seq
	dumpLog := fmt.Sprintf(" paxos: %d, Receive DecidedArgs, N: %d, V.Seq: %d", id, args.N, seq)
	log.Printf(dumpLog)
}

func (reply *DecidedReply) dump(logLevel int, id int) {
	if logLevel < INFO {
		return
	}
	dumpLog := fmt.Sprintf(" paxos: %d, Receive DecidedReply, N: %d", id, reply.N)
	log.Printf(dumpLog)
}

func (px *Paxos) Decided(n int, v interface{}) {
	args := &DecidedArgs{
		N: n,
		V: v,
	}
	for _, peer := range px.peers {
		go func(server string) {
			var reply DecidedReply
			call(server, "Paxos.DecidedReceive", args, &reply)
			ext := &DecidedExt{
				Args: args,
				Reply: &reply,
			}
			px.decidedReplyChan <- ext
		}(peer)
	}
}

func (px *Paxos) DecidedReceive(args *DecidedArgs, reply *DecidedReply) error {
	px.decidedArgsChan <- args
	replyInternal, ok := <- px.decidedReplyInterChan
	if !ok || replyInternal == nil {
		log.Fatal("DecidedReceive fatal.")
	} else {
		*reply = *replyInternal
	}
	return nil
}

func (px *Paxos) CommandReceive(args *CommandArgs, reply *CommandReply) error {
	px.commandArgsChan <- args
	internelReply, ok := <- px.commandReplyChan
	if !ok || reply == nil {
		log.Fatal("Start fatal.")
	}
	*reply = *internelReply
	return nil
}

//
// the application wants paxos to start agreement on
// instance seq, with proposed value v.
// Start() returns right away; the application will
// call Status() to find out if/when agreement
// is reached.
//
func (px *Paxos) Start(seq int, v interface{}) {
	// Your code here.
	px.commandArgsChan <- &CommandArgs{
		Seq: seq,
		V: v,
		Name: START,
	}
	reply, ok := <- px.commandReplyChan
	if !ok || reply == nil {
		log.Fatal("Start fatal.")
	}
	return
}

//
// the application on this machine is done with
// all instances <= seq.
//
// see the comments for Min() for more explanation.
//
func (px *Paxos) Done(seq int) {
	// Your code here.
	px.commandArgsChan <- &CommandArgs{
		Seq: seq,
		Name: DONE,
	}
	reply, ok := <- px.commandReplyChan
	if !ok || reply == nil {
		log.Fatal("Start fatal.")
	}
	return
}

//
// the application wants to know the
// highest instance sequence known to
// this peer.
//
func (px *Paxos) Max() int {
	// Your code here.
	px.commandArgsChan <- &CommandArgs{
		Name: MAX,
	}
	reply, ok := <- px.commandReplyChan
	if !ok || reply == nil {
		log.Fatal("Start fatal.")
	}
	return reply.Seq
}

//
// Min() should return one more than the minimum among z_i,
// where z_i is the highest number ever passed
// to Done() on peer i. A peers z_i is -1 if it has
// never called Done().
//
// Paxos is required to have forgotten all information
// about any instances it knows that are < Min().
// The point is to free up memory in long-running
// Paxos-based servers.
//
// Paxos peers need to exchange their highest Done()
// arguments in order to implement Min(). These
// exchanges can be piggybacked on ordinary Paxos
// agreement protocol messages, so it is OK if one
// peers Min does not reflect another Peers Done()
// until after the next instance is agreed to.
//
// The fact that Min() is defined as a minimum over
// *all* Paxos peers means that Min() cannot increase until
// all peers have been heard from. So if a peer is dead
// or unreachable, other peers Min()s will not increase
// even if all reachable peers call Done. The reason for
// this is that when the unreachable peer comes back to
// life, it will need to catch up on instances that it
// missed -- the other peers therefor cannot forget these
// instances.
//
/*
func (px *Paxos) Min() int {
	// You code here.
	px.commandArgsChan <- &CommandArgs{
		Name: MIN,
	}
	reply, ok := <- px.commandReplyChan
	if !ok || reply == nil {
		log.Fatal("Start fatal.")
	}
	return reply.Seq
}
*/
func (px *Paxos) Min() int {
	args := &CommandArgs{
		Name: MIN,
	}
	min := math.MaxInt32
	for _, peer := range px.peers {
		var reply CommandReply
		call(peer, "Paxos.CommandReceive", args, &reply)
		if reply.Seq < min {
			min = reply.Seq
		}
	}
	return min
}
//
// the application wants to know whether this
// peer thinks an instance has been decided,
// and if so what the agreed value is. Status()
// should just inspect the local peer state;
// it should not contact other Paxos peers.
//
func (px *Paxos) Status(seq int) (Fate, interface{}) {
	// Your code here.
	px.commandArgsChan <- &CommandArgs{
		Seq: seq,
		Name: STATUS,
	}
	reply, ok := <- px.commandReplyChan
	if !ok || reply == nil {
		log.Fatal("Start fatal.")
	}
	return reply.State, reply.V
}

//
// tell the peer to shut itself down.
// for testing.
// please do not change these two functions.
//
func (px *Paxos) Kill() {
	atomic.StoreInt32(&px.dead, 1)
	if px.l != nil {
		px.l.Close()
	}
	px.exitChan <- true
}

//
// has this peer been asked to shut down?
//
func (px *Paxos) isdead() bool {
	return atomic.LoadInt32(&px.dead) != 0
}

// please do not change these two functions.
func (px *Paxos) setunreliable(what bool) {
	if what {
		atomic.StoreInt32(&px.unreliable, 1)
	} else {
		atomic.StoreInt32(&px.unreliable, 0)
	}
}

func (px *Paxos) isunreliable() bool {
	return atomic.LoadInt32(&px.unreliable) != 0
}

//
// the application wants to create a paxos peer.
// the ports of all the paxos peers (including this one)
// are in peers[]. this servers port is peers[me].
//
func Make(peers []string, me int, rpcs *rpc.Server) *Paxos {
	px := &Paxos{}
	px.peers = peers
	px.me = me
	px.id = id
	id ++
	px.logLevel = ERROR

	// Your initialization code here.
	px.n_p = 0
	px.v_p = nil
	px.n_a = 0
	px.v_a = nil
	px.rounding = false
	px.decidedInstances = make([]*InstanceState, 0)

	px.instanceStates = make([]*InstanceState, 0)
	px.instanceIndex = 0

	px.proposeN = 0
	px.proposeV = nil
	px.prepareVote = nil
	px.prepareVoteCounter = 0
	px.acceptVoteCounter = 0
	px.prepared = false
	px.accepted = false
	px.decided = true

	px.prepareReplyChan = make(chan *PrepareExt)
	px.prepareArgsChan = make(chan *PrepareArgs)
	px.prepareReplyInterChan = make(chan *PrepareReply)
	px.acceptReplyChan = make(chan *AcceptExt)
	px.acceptArgsChan = make(chan *AcceptArgs)
	px.acceptReplyInterChan = make(chan *AcceptReply)
	px.decidedReplyChan = make(chan *DecidedExt)
	px.decidedArgsChan = make(chan *DecidedArgs)
	px.decidedReplyInterChan = make(chan *DecidedReply)
	px.commandReplyChan = make(chan *CommandReply)
	px.commandArgsChan = make(chan *CommandArgs)
	px.exitChan = make(chan bool)
	px.timer = time.NewTicker(time.Millisecond * 200)

	go px.eventLoop()

	if rpcs != nil {
		// caller will create socket &c
		rpcs.Register(px)
	} else {
		rpcs = rpc.NewServer()
		rpcs.Register(px)

		// prepare to receive connections from clients.
		// change "unix" to "tcp" to use over a network.
		os.Remove(peers[me]) // only needed for "unix"
		l, e := net.Listen("unix", peers[me])
		if e != nil {
			log.Fatal("listen error: ", e)
		}
		px.l = l

		// please do not change any of the following code,
		// or do anything to subvert it.

		// create a thread to accept RPC connections
		go func() {
			for px.isdead() == false {
				conn, err := px.l.Accept()
				if err == nil && px.isdead() == false {
					if px.isunreliable() && (rand.Int63()%1000) < 100 {
						// discard the request.
						conn.Close()
					} else if px.isunreliable() && (rand.Int63()%1000) < 200 {
						// process the request but force discard of reply.
						c1 := conn.(*net.UnixConn)
						f, _ := c1.File()
						err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
						if err != nil {
							fmt.Printf("shutdown: %v\n", err)
						}
						atomic.AddInt32(&px.rpcCount, 1)
						go rpcs.ServeConn(conn)
					} else {
						atomic.AddInt32(&px.rpcCount, 1)
						go rpcs.ServeConn(conn)
					}
				} else if err == nil {
					conn.Close()
				}
				if err != nil && px.isdead() == false {
					fmt.Printf("Paxos(%v) accept: %v\n", me, err.Error())
				}
			}
		}()
	}

	return px
}

func (px *Paxos) tryDecidedInstance(dicidedInstance *InstanceState) {
	instannceDecided := false
	for _, item := range px.decidedInstances {
		if item.instance.Seq == dicidedInstance.instance.Seq {
			instannceDecided = true
			break
		}
	}
	if instannceDecided == false {
		px.decidedInstances = append(px.decidedInstances, dicidedInstance)
	}
}

func (px *Paxos) tryGetInstance(seq int) *InstanceState {
	for _, item := range px.decidedInstances {
		if item.instance.Seq == seq {
			return item
		}
	}
	return nil
}

func (px *Paxos) handlePrepareVote(args *PrepareArgs) *PrepareReply {
	args.dump(px.logLevel, px.id)
	px.dump("Before handlePrepareVote", px.logLevel)
	defer func() {
		px.dump("After handlePrepareVote", px.logLevel)
	}()
	px.rounding = true
	var reply PrepareReply
	seq := args.V.(Instance).Seq
	instance := px.tryGetInstance(seq)
	if instance != nil {
		px.n_p = args.N
		px.v_p = args.V
		reply.N = args.N
		reply.N_a = 1
		reply.V_a = instance.instance
	} else {
		if args.N > px.n_p {
			px.n_p = args.N
			px.v_p = args.V
			reply.N = args.N
			reply.N_a = px.n_a
			reply.V_a = px.v_a
		} else {
			reply.N = args.N
			reply.N_a = -1
		}
	}
	return &reply
}

func (px *Paxos) handlePrepareReply(ext *PrepareExt) {
	ext.Reply.dump(px.logLevel, px.id)
	px.dump("Before handlePrepareReply", px.logLevel)
	defer func() {
		px.dump("After handlePrepareReply", px.logLevel)
	}()
	if px.prepared == true {
		return
	}
	reply := ext.Reply
	if reply.N != px.proposeN {
		return
	}
	if reply.N_a == -1 {
		return
	}
	if reply.N_a > 0 {
		if px.prepareVote == nil {
			px.prepareVote = reply
		} else if reply.N_a > px.prepareVote.N_a {
			px.prepareVote = reply
		}
	}
	px.prepareVoteCounter ++
	if px.prepareVoteCounter > len(px.peers) / 2 {
		var v_accept interface{}
		if px.prepareVote != nil {
			v_accept = px.prepareVote.V_a
		} else {
			v_accept = px.proposeV
		}
		px.proposeV = v_accept
		px.Accept(px.proposeN, px.proposeV)
		px.prepared = true
	}
}

func (px *Paxos) handleAcceptVote(args *AcceptArgs) *AcceptReply {
	args.dump(px.logLevel, px.id)
	px.dump("Before handleAcceptVote", px.logLevel)
	defer func() {
		px.dump("After handleAcceptVote", px.logLevel)
	}()
	var reply AcceptReply
	if px.rounding == false {
		reply.N = -1
		return &reply
	}
	if args.N >= px.n_p {
		px.n_p = args.N
		px.n_a = args.N
		px.v_a = args.V
		reply.N = args.N
	} else {
		reply.N = -1
	}
	return &reply
}

func (px *Paxos) handleAcceptReply(ext *AcceptExt) {
	ext.Reply.dump(px.logLevel, px.id)
	px.dump("Before handleAcceptReply", px.logLevel)
	defer func() {
		px.dump("After handleAcceptReply", px.logLevel)
	}()
	if px.accepted == true {
		return
	}
	reply := ext.Reply
	if reply.N != px.proposeN {
		return
	}
	if reply.N == -1 {
		return
	}
	px.acceptVoteCounter ++
	if px.acceptVoteCounter > len(px.peers)/2 {
		px.Decided(px.proposeN, px.proposeV)
		px.accepted = true
	}
}

func (px *Paxos) handleDecided(args *DecidedArgs) *DecidedReply {
	args.dump(px.logLevel, px.id)
	px.dump("Before handleDecided", px.logLevel)
	defer func() {
		px.dump("After handleDecided", px.logLevel)
	}()
	instance := args.V.(Instance)
	px.tryDecidedInstance(&InstanceState{
		instance: instance,
		state:    Decided,
	})
	var reply DecidedReply
	reply.N = args.N
	px.n_p = 0
	px.v_p = nil
	px.n_a = 0
	px.v_a = nil
	px.rounding = false
	return &reply
}

func (px *Paxos) handleDecidedReply(ext *DecidedExt) {
	ext.Reply.dump(px.logLevel, px.id)
	px.dump("Before handleDecidedReply", px.logLevel)
	defer func() {
		px.dump("After handleDecidedReply", px.logLevel)
	}()
	if px.decided == true {
		return
	}
	reply := ext.Reply
	if reply.N != px.proposeN {
		return
	}
	px.decided = true
	if px.prepareVote != nil {
		instance := px.prepareVote.V_a.(Instance)
		if px.prepareVote.N_a == 1 {
			decidedInstance := px.instanceStates[px.instanceIndex]
			decidedInstance.state = Decided
			decidedInstance.instance = instance
		}
		px.tryDecidedInstance( &InstanceState{
			instance: instance,
			state: Decided,
		})
	} else {
		decidedInstance := px.instanceStates[px.instanceIndex]
		decidedInstance.state = Decided
		px.tryDecidedInstance(decidedInstance)
	}
}

func (px *Paxos) handleCommand(args *CommandArgs) *CommandReply {
	/*
	args.dump(px.logLevel, px.id)
	px.dump("Before handleCommand", px.logLevel)
	defer func() {
		px.dump("After handleCommand", px.logLevel)
	}()
	*/
	var reply CommandReply
	switch args.Name {
	case START:
		state := &InstanceState{
			instance: Instance{
				Seq: args.Seq,
				V: args.V,
			},
			state: Pending,
		}
		px.instanceStates = append(px.instanceStates, state)
		return &reply
	case DONE:
		seq := args.Seq
		for _, item := range px.decidedInstances {
			if item.instance.Seq <= seq {
				item.state = Forgotten
				item.instance = Instance{
					Seq: item.instance.Seq,
				}
			}
		}
		return &reply
	case MAX:
		max := 0
		if len(px.decidedInstances) > 0 {
			max = px.decidedInstances[len(px.decidedInstances) - 1].instance.Seq
		}
		for _, item := range px.decidedInstances {
			if item.state == Decided && item.instance.Seq > max {
				max = item.instance.Seq
			}
		}
		reply.Seq = max
		return &reply
	case MIN:
		min := math.MaxInt32
		for _, item := range px.decidedInstances {
			if item.state == Decided && item.instance.Seq < min {
				min = item.instance.Seq
			}
		}
		if min == math.MaxInt32 {
			min = -1
		}
		reply.Seq = min
		return &reply
	case STATUS:
		seq := args.Seq
		for _, item := range px.decidedInstances {
			if item.instance.Seq == seq {
				reply.State = item.state
				reply.V = item.instance
			}
		}
		return &reply
	}
	return &reply
}

func (px *Paxos) eventLoop() {
	for {
		select {
		case <- px.timer.C:
			for (len(px.instanceStates) > px.instanceIndex) {
				instanceState := px.instanceStates[px.instanceIndex]
				if instanceState.state == Pending {
					if px.decided == true || px.roundTimeout >= 5 {
						px.Prepare(instanceState.instance)
						px.roundTimeout = 0
					} else {
						px.roundTimeout ++
					}
					break
				} else {
					px.instanceIndex ++
				}
			}
		case prepareArgs, ok :=  <- px.prepareArgsChan:
			if !ok || prepareArgs == nil {
				break
			}
			reply := px.handlePrepareVote(prepareArgs)
			px.prepareReplyInterChan <- reply
		case prepareReply, ok := <- px.prepareReplyChan:
			if !ok || prepareReply == nil {
				break
			}
			px.handlePrepareReply(prepareReply)
		case acceptArgs, ok := <- px.acceptArgsChan:
			if !ok || acceptArgs == nil {
				break
			}
			reply := px.handleAcceptVote(acceptArgs)
			px.acceptReplyInterChan <- reply
		case acceptReply, ok := <- px.acceptReplyChan:
			if !ok || acceptReply == nil {
				break
			}
			px.handleAcceptReply(acceptReply)
		case decidedArgs, ok := <- px.decidedArgsChan:
			if !ok || decidedArgs == nil {
				break
			}
			reply := px.handleDecided(decidedArgs)
			px.decidedReplyInterChan <- reply
		case decidedReply, ok := <- px.decidedReplyChan:
			if !ok || decidedReply == nil {
				break
			}
			px.handleDecidedReply(decidedReply)
		case commandArgs, ok := <- px.commandArgsChan:
			if !ok || commandArgs == nil {
				break
			}
			reply := px.handleCommand(commandArgs)
			px.commandReplyChan <- reply
		case exit, ok := <- px.exitChan:
			if !ok || exit != true {
				break
			}
			px.timer.Stop()
			return
		}
	}
}
