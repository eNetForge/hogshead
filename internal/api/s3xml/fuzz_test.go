package s3xml

import (
	"strings"
	"testing"
)

func FuzzUnmarshal(f *testing.F) {
	f.Add(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"x"</ETag></Part></CompleteMultipartUpload>`)
	f.Add(`<Delete><Object><Key>k</Key></Object></Delete>`)
	f.Add(`<LifecycleConfiguration><Rule><Status>Enabled</Status></Rule></LifecycleConfiguration>`)
	f.Add(`<`)
	f.Fuzz(func(t *testing.T, s string) {
		var c CompleteMultipartUpload
		_ = Unmarshal(strings.NewReader(s), &c, 1<<16)
		var d Delete
		_ = Unmarshal(strings.NewReader(s), &d, 1<<16)
		var l LifecycleConfiguration
		_ = Unmarshal(strings.NewReader(s), &l, 1<<16)
	})
}
