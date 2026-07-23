package handler_test

import (
	"bytes"
	"encoding/xml"
	"testing"

	"github.com/go-faster/fs/internal/core/handler"
)

// The S3 request-body XML parsers decode attacker-controlled bodies. Decoding
// must fail gracefully on any input, never panic. These fuzz the exact decode
// boundary each handler uses (xml.NewDecoder(body).Decode(&struct)).

func FuzzDecodeCompleteMultipartUpload(f *testing.F) {
	f.Add([]byte(`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"a"</ETag></Part></CompleteMultipartUpload>`))
	f.Add([]byte(`<CompleteMultipartUpload></CompleteMultipartUpload>`))
	f.Add([]byte(``))
	f.Add([]byte(`<CompleteMultipartUpload><Part><PartNumber>-1</PartNumber></Part></CompleteMultipartUpload>`))

	f.Fuzz(func(t *testing.T, body []byte) {
		var req handler.CompleteMultipartUploadXML

		_ = xml.NewDecoder(bytes.NewReader(body)).Decode(&req)
	})
}

func FuzzDecodeTagging(f *testing.F) {
	f.Add([]byte(`<Tagging><TagSet><Tag><Key>k</Key><Value>v</Value></Tag></TagSet></Tagging>`))
	f.Add([]byte(`<Tagging><TagSet></TagSet></Tagging>`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, body []byte) {
		var doc handler.Tagging

		_ = xml.NewDecoder(bytes.NewReader(body)).Decode(&doc)
	})
}

func FuzzDecodeDeleteObjects(f *testing.F) {
	f.Add([]byte(`<Delete><Object><Key>k</Key></Object><Quiet>true</Quiet></Delete>`))
	f.Add([]byte(`<Delete></Delete>`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, body []byte) {
		var req handler.DeleteObjectsRequest

		_ = xml.NewDecoder(bytes.NewReader(body)).Decode(&req)
	})
}
