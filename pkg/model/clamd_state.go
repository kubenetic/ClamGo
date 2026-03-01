package model

type ClamdState string

const (
	StateValid   ClamdState = "VALID"
	StateInvalid            = "INVALID"
	StateExit               = "EXIT"
	StateUnknown            = "??"
)
