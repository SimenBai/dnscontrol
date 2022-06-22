package powerdns

import (
	"fmt"
	"github.com/mittwald/go-powerdns/apis/zones"
	"strings"
	"testing"
)
import "github.com/stretchr/testify/assert"

func TestToRecordConfig(t *testing.T) {
	record := zones.Record{
		Content: "\"simple\"",
	}
	recordConfig, err := toRecordConfig("example.com", record, 120, "test", "TXT")

	if assert.NoError(t, err) && assert.NotNil(t, recordConfig) {
		assert.Equal(t, "test.example.com", recordConfig.NameFQDN)
		assert.Equal(t, "\"simple\"", recordConfig.String())
		assert.Equal(t, uint32(120), recordConfig.TTL)
		assert.Equal(t, "TXT", recordConfig.Type)
	}

	largeContent := fmt.Sprintf("\"%s\" \"%s\"", strings.Repeat("A", 300), strings.Repeat("B", 300))
	largeRecord := zones.Record{
		Content: largeContent,
	}
	recordConfig, err = toRecordConfig("example.com", largeRecord, 5, "large", "TXT")

	if assert.NoError(t, err) && assert.NotNil(t, recordConfig) {
		assert.Equal(t, "large.example.com", recordConfig.NameFQDN)
		assert.Equal(t, largeContent, recordConfig.String())
		assert.Equal(t, uint32(5), recordConfig.TTL)
		assert.Equal(t, "TXT", recordConfig.Type)
	}
}
