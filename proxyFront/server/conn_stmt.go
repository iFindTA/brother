package server

import "brother/sqlparser"

type Stmt struct {
	id 				uint32

	params 				int
	columns				int

	args				[]interface{}

	s				sqlparser.Statement
	sql				string
}
