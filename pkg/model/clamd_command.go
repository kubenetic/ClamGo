package model

type ClamDCommand string

const (
	CmdPing         ClamDCommand = "PING"
	CmdVersion      ClamDCommand = "VERSION"
	CmdStats        ClamDCommand = "STATS"
	CmdScan         ClamDCommand = "SCAN"
	CmdStartSession ClamDCommand = "IDSESSION"
	CmdEndSession   ClamDCommand = "END"
)
