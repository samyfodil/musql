package replication

// OpKind indicates the type of operation.
type OpKind uint8

const (
	OpInsert OpKind = iota
	OpUpdate
	OpDelete
)

// SQLite type constants for Cell.Type field.
const (
	TypeNull  = byte(0)
	TypeInt   = byte(1)
	TypeFloat = byte(2)
	TypeText  = byte(3)
	TypeBlob  = byte(4)
)

// Cell represents a single column value in an operation.
type Cell struct {
	Col  string // column name
	Type byte   // SQLite storage class: TypeNull, TypeInt, TypeFloat, TypeText, TypeBlob
	Val  []byte // encoded value
}

// Op represents a single operation (Insert/Update/Delete) captured from the database.
type Op struct {
	Site  string // originating site
	Seq   uint64 // per-site sequence number for dedup
	HLC   HLC    // hybrid logical clock timestamp
	Tbl   string // table name
	PK    []byte // encoded primary key
	Kind  OpKind // operation type
	Cells []Cell // changed cells (all for Insert, changed only for Update, none for Delete)
}

// VersionVector represents the per-site maximum sequence numbers for dedup and anti-entropy.
type VersionVector map[string]uint64

// Clone returns a deep copy of the VersionVector.
func (vv VersionVector) Clone() VersionVector {
	result := make(VersionVector, len(vv))
	for site, seq := range vv {
		result[site] = seq
	}
	return result
}
