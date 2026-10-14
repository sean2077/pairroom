package model

// EqualMetadata compares immutable attachment facts. CreatedAt identifies an
// instant: SQL and JSON may represent it with different time.Location values.
// Every other field stays exact, including any future fields on Attachment.
func (a Attachment) EqualMetadata(b Attachment) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return false
	}
	b.CreatedAt = a.CreatedAt
	return a == b
}
