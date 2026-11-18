package deploy_test

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
)

func TestManifestDecoding(t *testing.T) {
	files, err := filepath.Glob("*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("manifest discovery: %v (%d files)", err, len(files))
	}
	decoder := serializer.NewCodecFactory(scheme.Scheme, serializer.EnableStrict).UniversalDeserializer()
	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.ReplaceAll(data, []byte("REPLACE_WITH_BASE64_CA"), []byte("Y2VydA=="))
			for reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data))); ; {
				document, err := reader.Read()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := decoder.Decode(document, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
