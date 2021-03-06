package pbservice

const (
	OK             = "OK"
	ErrNoKey       = "ErrNoKey"
	Handling       = "Handling"
	ErrWrongServer = "ErrWrongServer"
)

type Err string

const (
	IDLE                = iota
	ASSIGN_PRIMARY
	CONFIRM_PRIMARY
	BACKUP
)

const (
	REQUEST            = iota
	HANDLING
	HANDLED
)

// Put or Append
type PutAppendArgs struct {
	Key   string
	Value string
	// You'll have to add definitions here.
	From  string
	Number int
	Op    string
	// Field names must start with capital letters,
	// otherwise RPC will break.
}

type PutAppendReply struct {
	Err Err
}

type GetArgs struct {
	Key string
	// You'll have to add definitions here.
}

type GetReply struct {
	Err   Err
	Value string
}


// Your RPC definitions here.
type CopyArgs struct {
	Data   map[string]string
}

type CopyReply struct {
	Err  Err
}