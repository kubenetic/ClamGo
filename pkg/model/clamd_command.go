package model

type ClamDCommand string

const (
    CmdPing         ClamDCommand = "PING"
    CmdVersion                   = "VERSION"
    CmdStats                     = "STATS"
    CmdScan                      = "SCAN"
    CmdStartSession              = "IDSESSION"
    CmdEndSession                = "END"
)
