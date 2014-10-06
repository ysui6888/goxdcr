package base

import ()

//constants
var DefaultConnectionSize = 20
var DefaultPoolName = "default"

//constants for adminport
var AdminportUrlPrefix = "/"
var AdminportNumber = 12100
// AdminportReadTimeout timeout, in milliseconds, is read timeout for
// golib's http server.
var AdminportReadTimeout = 0
// AdminportWriteTimeout timeout, in milliseconds, is write timeout for
// golib's http server.
var AdminportWriteTimeout = 0

//outgoing nozzle type
type XDCROutgoingNozzleType int

const (
	Xmem XDCROutgoingNozzleType = iota
	Capi XDCROutgoingNozzleType = iota
)

const (
	PIPELINE_SUPERVISOR_SVC string = "PipelineSupervisor"
	CHECKPOINT_MGR_SVC string = "CheckpointManager"
)

// constants for integer parsing
var ParseIntBase    = 10
var ParseIntBitSize = 64
