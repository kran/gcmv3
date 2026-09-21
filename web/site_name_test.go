package web

import (
	"os"
	"path/filepath"
	"testing"
)

const nameSiteYAML = `
name: 乐至期智库
types:
  article:
    capabilities: { addressable: true }
    fields:
      - { name: name, kind: text }
`

// site.yaml: 站点名进 Site.Name()。
func TestSiteNameFromSiteYAML(t *testing.T) {
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "site.yaml"), []byte(nameSiteYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	site, err := Open(basedir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = site.Close() }()
	if site.Name() != "乐至期智库" {
		t.Fatalf("站点名: %q", site.Name())
	}
	if _, ok := site.Types().Type("article"); !ok {
		t.Fatal("types 子树该照常加载")
	}
}

// 没有 site.yaml ⇒ 起不来（不给兜底; 站点必须有一份声明）。
func TestSiteYAMLRequired(t *testing.T) {
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "site.yaml"), []byte("types: {}\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(basedir)
	if err == nil {
		t.Fatal("只有 site.yaml 该起不来（改用 site.yaml 了）")
	}
}

// 顶层键拼错 ⇒ 当场报错（KnownFields 在 web 这层也开着）。
func TestSiteYAMLStrict(t *testing.T) {
	basedir := t.TempDir()
	err := os.WriteFile(filepath.Join(basedir, "site.yaml"), []byte("nmae: 打错了\ntypes: {}\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(basedir)
	if err == nil {
		t.Fatal("顶层键拼错该报错（不静默忽略）")
	}
}
