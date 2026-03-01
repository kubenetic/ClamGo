package model

type ClamdState string

const (
	StateValid   ClamdState = "VALID"
	StateInvalid ClamdState = "INVALID"
	StateExit    ClamdState = "EXIT"
	StateUnknown ClamdState = "??"
)
