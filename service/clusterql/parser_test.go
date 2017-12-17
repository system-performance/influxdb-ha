package clusterql

import (
	"testing"
	"strings"
	"github.com/stretchr/testify/assert"
)

func TestParser_ParseShow(t *testing.T) {
	lang := CreateLanguage()

	stmt, err := NewParser(strings.NewReader(`SHOW PARTITION KEYS ON "mydb"`), lang).Parse()
	assert.NoError(t, err)
	assert.IsType(t, ShowPartitionKeysStatement{}, stmt)
	assert.Equal(t, "mydb", stmt.(ShowPartitionKeysStatement).Database)
}
