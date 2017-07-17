package proxyBack

import (
	"net"
	"brother/mysql"
	"strings"
	"errors"
	f"fmt"
	"bytes"
	"encoding/binary"
)

//proxy <-> mysql server
type Conn struct {
	conn 				net.Conn // 与mysql server之间真正的TCP长连接

	pkg 				*mysql.PacketIO

	addr				string
	user				string
	passwd				string
	db				string

	capability			uint32

	status				uint16

	collation			mysql.CollationId
	charset				string
	salt				[]byte

	pushTimestamp			int64
	pkgErr				error
}

func (c *Conn) Connect(addr, user, passwd, db string) error {
	c.addr = addr
	c.user = user
	c.passwd = passwd
	c.db = db

	//use utf-8
	c.collation = mysql.DEFAULT_COLLATION_ID
	c.charset = mysql.DEFAULT_CHARSET

	return c.ReConnect()
}

func (c *Conn) ReConnect() error {
	if c.conn != nil {
		c.conn.Close()
	}
	//三种连接 mysql数据库 的方法 这里默认使用tcp
	n := "tcp"
	if strings.Contains(c.addr, "/") {
		n = "unix"
	}

	netConn, err := net.Dial(n, c.addr)
	if err != nil {
		return err
	}

	tcpConn := netConn.(*net.TCPConn)
	//SetNoDelay controls whether the operating system should delay packet transmission
	// in hopes of sending fewer packets (Nagle's algorithm).
	// The default is true (no delay),
	// meaning that data is sent as soon as possible after a Write.
	//I set this option false.
	tcpConn.SetNoDelay(false)
	tcpConn.SetKeepAlive(true)//保持长连接
	c.conn = tcpConn
	c.pkg = mysql.NewPacketIO(tcpConn)

	if err := c.readInitialHandshake(); err != nil {
		c.conn.Close()
		return err
	}

	if err:= c.writeAuthHandshake(); err != nil {
		c.conn.Close()
		return err
	}

	if _, err := c.readOK(); err != nil {
		c.conn.Close()
		return err
	}

	//we must always use auto-commit
	if !c.IsAutoCommit() {
		if _, err := c.exec("set autocommit = 1"); err != nil {
			c.conn.Close()
			return err
		}
	}

	return nil
}

func (c *Conn) Close() error {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.salt = nil
		c.pkgErr = nil
	}
	return nil
}

/**
 * #################################### proxy <-> mysql server Conn Events ##################################
 */
func (c *Conn) readPacket() ([]byte, error) {
	d, err := c.pkg.ReadPacket()
	c.pkgErr = err
	return d, err
}

func (c *Conn) writePacket(data []byte) error {
	err := c.pkg.WritePacket(data)
	c.pkgErr = err
	return err
}

func (c *Conn) readInitialHandshake() error {
	data, err := c.readPacket()
	if err != nil{
		 return err
	}

	if data[0] == mysql.ERR_HEADER {
		return errors.New("read initial handshake error.")
	}

	if data[0] < mysql.MinProtocolVersion {
		return f.Errorf("invalid protocol version %d, must >= 10", data[0])
	}

	//skip mysql version and connection id
	//mysql version end with ox00
	//connection id length is 4
	pos := 1 + bytes.IndexByte(data[1:], 0x00) +1 + 4

	c.salt = append(c.salt, data[pos:pos+8]...)

	//skip filter
	pos += 8 + 1

	//capability lower 2 bytes
	c.capability = uint32(binary.LittleEndian.Uint16(data[pos : pos + 2]))
	
	pos += 2

	if len(data) > pos {
		//skip server charset
		//c.charset = data[pos]
		pos += 1

		c.status = binary.LittleEndian.Uint16(data[pos : pos + 2])
		pos += 2

		c.capability = uint32(binary.LittleEndian.Uint16(data[pos : pos + 2]))<<16 | c.capability

		pos += 2

		//skip auth data len or [00]
		//skip reserved (all [00])
		pos += 10 + 1

		//the documentation is ambiguous about the length.
		//the official python library uses the fixed length 12
		//mysql-proxy also use 12
		//which is not documented but seems to work.
		c.salt = append(c.salt, data[pos : pos + 12]...)
	}

	return nil
}

func (c *Conn) writeAuthHandshake() error {
	//Adjust client capability flags based on server support
	capability := mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_SECURE_CONNECTION | mysql.CLIENT_LONG_PASSWORD | mysql.CLIENT_TRANSACTIONS  | mysql.CLIENT_LONG_FLAG

	capability &= c.capability

	//packet length
	//capability 4
	//max-packet size 4
	//charset 1
	//reserved all[0] 23
	length := 4 + 4 + 1 + 23

	//username
	length += len(c.user) + 1

	//we only support secure connection
	auth := mysql.CalcPassword(c.salt, []byte(c.passwd))

	length += 1 + len(auth)

	if len(c.db) > 0 {
		capability |= mysql.CLIENT_CONNECT_WITH_DB
		length += len(c.db) + 1
	}

	c.capability = capability

	data := make([]byte, length + 4)

	//capability [32 bit]
	data[4] = byte(capability)
	data[5] = byte(capability >> 8)
	data[6] = byte(capability >> 16)
	data[7] = byte(capability >> 24)

	//maxPacketSize [32 bit] (none)
	//data[8] = 0x00
	//data[9] = 0x00
	//data[10] = 0x00
	//data[11] = 0x00

	//charset [1 byte]
	data[12] = byte(c.collation)

	//filter [23 bytes] (all 0x00)
	pos := 13 + 23

	//user [null terminated string]
	if len(c.user) > 0 {
		pos += copy(data[pos:], c.user)
	}
	//data[pos] = 0x00
	pos++

	//auth [length encoded integer]
	data[pos] = byte(len(auth))
	pos += 1 + copy(data[pos+1:], auth)

	//db [null terminated string]
	if len(c.db) > 0 {
		pos += copy(data[pos:], c.db)
		//data[pos] = 0x00
	}

	return c.writePacket(data)
}

func (c *Conn) writeCommand(command byte) error {
	c.pkg.Sequence = 0

	return c.writePacket([]byte{
		0x01,//1 byte long
		0x00,
		0x00,
		0x00,//sequence
		command,
	})
}

func (c *Conn) writeCommandBuf(command byte, arg []byte) error {
	c.pkg.Sequence = 0

	length := len(arg) + 1

	data := make([]byte, length + 4)

	data[4] = command

	copy(data[5:], arg)

	return c.writePacket(data)
}

func (c *Conn) writeCommandStr(command byte, arg string) error {
	c.pkg.Sequence = 0

	length := len(arg) + 1

	data := make([]byte, length + 4)

	data[4] = command

	copy(data[5:], arg)

	return c.writePacket(data)
}

func (c *Conn) writeCommandUint32(command byte, arg uint32) error {
	c.pkg.Sequence = 0
	return c.writePacket([]byte{
		0x05, //5 bytes long
		0x00,
		0x00,
		0x00, //sequence
		command,
		byte(arg),
		byte(arg >> 8),
		byte(arg >> 16),
		byte(arg >> 24),
	})
}

func (c *Conn) writeCommandStrStr(command byte, arg1, arg2 string) error {
	c.pkg.Sequence = 0

	data := make([]byte, 4, 6 + len(arg1) + len(arg2))

	data = append(data, command)
	data = append(data, arg1...)
	data = append(data, 0)
	data = append(data, arg2...)

	return c.writePacket(data)
}

func (c *Conn) Ping() error {
	if err := c.writeCommand(mysql.COM_PING); err != nil{
		return err
	}
	if _, err := c.readOK(); err != nil {
		return err
	}

	return nil
}

func (c *Conn) UseDB(dbName string) error {
	if c.db == dbName || len(dbName) == 0 {
		return nil
	}

	if err := c.writeCommandStr(mysql.COM_INIT_DB, dbName); err != nil {
		return err
	}

	if _, err := c.readOK(); err != nil {
		return err
	}

	c.db = dbName
	return nil
}

func (c *Conn) GetDB() string {
	return c.db
}

func (c *Conn) GetAddr() string {
	return c.addr
}

func (c *Conn) Execute(command string, args ...interface{}) (*mysql.Result, error) {
	if len(args) == 0 {
		return c.exec(command)
	} else {
		if s, err := c.Prepare(command); err != nil {
			return nil, err
		} else {
			var r *mysql.Result
			r, err = s.Execute(args...)
			s.Close()
			return r, err
		}
	}
}

func (c *Conn) ClosePrepare(id uint32) error {
	return c.writeCommandUint32(mysql.COM_STMT_CLOSE, id)
}

func (c *Conn) Begin() error {
	_, err := c.exec("begin")
	return err
}

func (c *Conn) Commit() error {
	_, err := c.exec("commit")
	return err
}

func (c *Conn) Rollback() error {
	_, err := c.exec("rollback")
	return err
}

func (c *Conn) SetAutoCommit(n uint8) error {
	if n == 0 {
		if _, err := c.exec("set autocommit = 0"); err != nil {
			c.conn.Close()
			return err
		}
	} else {
		if _, err := c.exec("set autocommit = 1"); err != nil {
			c.conn.Close()
			return err
		}
	}
	return nil
}

func (c *Conn) SetCharset(charset string, collation mysql.CollationId) error {
	charset = strings.Trim(charset, "\"'`")

	if collation == 0 {
		collation = mysql.CollationNames[mysql.Charsets[charset]]
	}

	if c.charset == charset && c.collation == collation {
		return nil
	}
	
	_, ok := mysql.CharsetIds[charset]
	if !ok {
		return f.Errorf("invalid charset %s.", charset)
	}
	
	_, ok = mysql.Collations[collation]
	if !ok {
		return f.Errorf("Invalid collation %s.", collation)
	}

	if _, err := c.exec(f.Sprintf("SET NAMES %s COLLATE %s", charset, mysql.Collations[collation])); err != nil{
		return err
	} else {
		c.collation = collation
		c.charset = charset
		return nil
	}
}

func (c *Conn) FieldList(table string, wildcard string) ([]*mysql.Field, error) {
	if err := c.writeCommandStrStr(mysql.COM_FIELD_LIST, table, wildcard); err != nil {
		return nil, err
	}

	data, err := c.readPacket()
	if err != nil {
		return nil, err
	}

	fs := make([]*mysql.Field, 0, 4)
	var fd *mysql.Field
	if data[0] == mysql.ERR_HEADER {
		return nil, c.handleErrorPacket(data)
	} else {
		for {
			if data, err = c.readPacket(); err != nil {
				return nil, err
			}

			//EOF Packet
			if c.isEOFPacket(data) {
				return fs, nil
			}

			if fd, err = mysql.FieldData(data).Parse(); err != nil {
				return nil, err
			}
			fs = append(fs, fd)
		}
	}

	return nil, f.Errorf("field list error")
}

func (c *Conn) readOK() (*mysql.Result, error) {
	data, err := c.readPacket()
	if err != nil {
		return nil, err
	}

	if data[0] == mysql.OK_HEADER {
		return c.handleOKPacket(data)
	} else if data[0] == mysql.ERR_HEADER {
		return nil, c.handleErrorPacket(data)
	} else {
		return nil, errors.New("invalid ok packet.")
	}
}

func (c *Conn) readResult(binary bool) (*mysql.Result, error) {
	data, err := c.readPacket()
	if err != nil {
		return nil, err
	}

	if data[0] == mysql.OK_HEADER {
		return c.handleOKPacket(data)
	} else if data[0] == mysql.ERR_HEADER {
		return nil, c.handleErrorPacket(data)
	} else if data[0] == mysql.LocalInFile_HEADER {
		return nil, mysql.ErrMalformPacket
	}

	return c.readResultset(data, binary)
}

func (c *Conn) readResultset(data []byte, binary bool) (*mysql.Result, error) {
	result := &mysql.Result{
		Status:0,
		InsertId:0,
		AffectedRows:0,
		Resultset:&mysql.Resultset{},
	}

	//column count
	count, _, n := mysql.LengthEncodedInt(data)

	if n - len(data) != 0 {
		return nil, mysql.ErrMalformPacket
	}

	result.Fields = make([]*mysql.Field, count)
	result.FieldNames = make(map[string]int, count)

	if err := c.readResultColums(result); err != nil {
		return nil, err
	}
	if err := c.readResultRows(result, binary); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Conn) readResultColums(result *mysql.Result) (err error) {
	var i int = 0
	var data []byte

	for {
		data, err = c.readPacket()
		if err != nil {
			return
		}

		//EOF Packet
		if c.isEOFPacket(data) {
			if c.capability&mysql.CLIENT_PROTOCOL_41 > 0 {
				//result.warnings = binary.LittleEndia.Uint16(date[1:])
				//TODO add strict_mode, warning will be treat as error
				result.Status = binary.LittleEndian.Uint16(data[3:])
				c.status = result.Status
			}

			if i != len(result.Fields) {
				err = mysql.ErrMalformPacket
			}

			return
		}

		result.Fields[i], err = mysql.FieldData(data).Parse()
		if err != nil {
			return
		}
		result.FieldNames[string(result.Fields[i].Name)] = i

		i++
	}
}

func (c *Conn) readResultRows(result *mysql.Result, isBinary bool) (err error) {
	var data []byte

	for {
		data, err = c.readPacket()
		if err != nil {
			return
		}

		//EOF Packet
		if c.isEOFPacket(data) {
			if c.capability&mysql.CLIENT_PROTOCOL_41 > 0 {
				//result.warnings = binary.LittleEndian.Uint16(data[1:])
				//TODO add strict_mode, warning will be treat as error
				result.Status = binary.LittleEndian.Uint16(data[3:])
				c.status = result.Status
			}
			break
		}

		result.RowDatas = append(result.RowDatas, data)
	}

	result.Values = make([][]interface{}, len(result.RowDatas))

	for i := range result.Values {
		result.Values[i], err = result.RowDatas[i].Parse(result.Fields, isBinary)
		if err != nil{
			return err
		}
	}
	return nil
}

func (c *Conn) readUntilEOF() (err error) {
	var data []byte
	for {
		data, err = c.readPacket()
		if err != nil {
			return err
		}
		//EOF Packet
		if c.isEOFPacket(data) {
			return
		}
	}
	return
}

func (c *Conn) handleOKPacket(data []byte) (*mysql.Result, error) {
	var n int
	var pos int = 1

	r:= new(mysql.Result)

	r.AffectedRows, _, n = mysql.LengthEncodedInt(data[pos:])
	pos += n
	r.InsertId, _, n = mysql.LengthEncodedInt(data[pos:])
	pos += n

	if c.capability&mysql.CLIENT_PROTOCOL_41 > 0 {
		r.Status = binary.LittleEndian.Uint16(data[pos:])
		c.status = r.Status
		pos += 2

		//TODO strict_mode, check warnings as error
		//warnings := binary.LittleEndian.Uint16(data[pos:])
		//pos += 2
	} else if c.capability&mysql.CLIENT_TRANSACTIONS > 0 {
		r.Status = binary.LittleEndian.Uint16(data[pos:])
		c.status = r.Status
		pos += 2
	}

	//info
	return r, nil
}

func (c *Conn) handleErrorPacket(data []byte) error {
	e := new(mysql.SqlError)

	var pos int = 1

	e.Code = binary.LittleEndian.Uint16(data[pos:])
	pos += 2

	if c.capability&mysql.CLIENT_PROTOCOL_41 > 0 {
		//skip '#'
		pos++
		e.State = string(data[pos : pos + 5])
		pos += 5
	}
	e.Message = string(data[pos:])

	return e
}

func (c *Conn) isEOFPacket(data []byte) bool {
	return data[0] == mysql.EOF_HEADER && len(data) <= 5
}

/**
 * ################################# proxy <-> mysql server getter ########################################
 */
func (c *Conn) IsAutoCommit() bool {
	return c.status&mysql.SERVER_STATUS_AUTOCOMMIT > 0
}

func (c *Conn) IsInTransaction() bool {
	return c.status&mysql.SERVER_STATUS_IN_TRANS > 0
}

func (c *Conn) GetCharset() string {
	return c.charset
}

/**
 * ############################## proxy <-> mysql server excute sql events ################################
 */

func (c *Conn) exec(query string) (*mysql.Result, error) {
	if err := c.writeCommandStr(mysql.COM_QUERY, query); err != nil {
		return nil, err
	}
	return c.readResult(false)
}