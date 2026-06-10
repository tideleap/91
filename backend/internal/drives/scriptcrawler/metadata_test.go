package scriptcrawler

import (
	"strings"
	"testing"
)

func TestExtractMetadataReadsCrawlerName(t *testing.T) {
	meta, err := ExtractMetadata(`
# comment
CRAWLER_NAME = "示例爬虫"
`)
	if err != nil {
		t.Fatalf("extract metadata: %v", err)
	}
	if meta.Name != "示例爬虫" {
		t.Fatalf("name = %q", meta.Name)
	}
}

func TestExtractMetadataRejectsMissingCrawlerName(t *testing.T) {
	_, err := ExtractMetadata(`print("hello")`)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "CRAWLER_NAME") {
		t.Fatalf("error = %v, want CRAWLER_NAME guidance", err)
	}
}

func TestExtractMetadataRejectsEmptyCrawlerName(t *testing.T) {
	_, err := ExtractMetadata(`CRAWLER_NAME = "  "`)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("error = %v, want empty-name error", err)
	}
}
