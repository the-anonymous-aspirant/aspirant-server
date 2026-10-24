package data_models

import (
	"bytes"
	"image"
	_ "image/gif"  // DecodeConfig for the pixel size of an uploaded GIF
	_ "image/jpeg" // DecodeConfig for the pixel size of an uploaded JPEG
	_ "image/png"  // DecodeConfig for the pixel size of an uploaded PNG
	"net/http"
	"regexp"
	"strings"

	"github.com/jinzhu/gorm"
)

// MaxCardAssetBytes caps one uploaded card asset. It matches the public
// nginx body limit in front of the server (aspirant-client default.conf,
// client_max_body_size 25m), so a larger file is refused at the edge before it
// reaches this cap; the cap here is what a direct call sees.
const MaxCardAssetBytes = 25 << 20 // 25 MiB

// Card asset kinds. Kind is what the editor and the viewer switch on; the
// sniffed MediaType is what the content route serves.
const (
	CardAssetKindImage = "image"
	CardAssetKindSVG   = "svg"
	CardAssetKindPDF   = "pdf"
	CardAssetKindFont  = "font"
)

// CardAsset is one file in the card editor's asset store (system_3 #8634,
// subtask #8642): a background, an icon, artwork, a font or a reference PDF
// that templates and cards may reference by id.
//
// The store is content-addressed. SHA256 is the identity: the same bytes
// uploaded twice resolve to the same row, and a row's bytes are never replaced,
// so a template that references asset 7 renders the same picture forever. The
// bytes live in the server's LocalStorage under StorageKey ("cardgen/<sha256>",
// no extension, so the storage layer's extension-based type is never used);
// MediaType is sniffed from the bytes at upload and is the only type the
// content route ever serves. Deletion is the gorm soft delete: the row leaves
// the listings, the bytes stay (another upload of the same content restores
// the row rather than writing a second copy).
type CardAsset struct {
	gorm.Model
	SHA256           string `json:"sha256" gorm:"column:sha256;type:varchar(64);not null;unique_index:idx_card_assets_sha256"`
	StorageKey       string `json:"-" gorm:"type:varchar(120);not null"`
	OriginalFilename string `json:"original_filename" gorm:"type:varchar(255);not null"`
	MediaType        string `json:"media_type" gorm:"type:varchar(80);not null"`
	Kind             string `json:"kind" gorm:"type:varchar(16);not null;index"`
	ByteSize         int64  `json:"byte_size" gorm:"not null"`
	PixelWidth       int    `json:"pixel_width"`
	PixelHeight      int    `json:"pixel_height"`
	UploadedByUserID uint   `json:"uploaded_by_user_id" gorm:"not null;index"`
}

func (CardAsset) TableName() string { return "card_assets" }

// CardAssetStorageKey is where the bytes of an asset with the given sha256 live
// in LocalStorage. The key is derived from the content, never from the caller,
// so no upload can name a path.
func CardAssetStorageKey(sha256Hex string) string { return "cardgen/" + sha256Hex }

// cardAssetRasterTypes are the sniffed types the content route may serve
// inline: a browser renders them as pictures and nothing else. Everything the
// store accepts beyond these (SVG, PDF, fonts) is served as an attachment.
var cardAssetRasterTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// CardAssetServesInline reports whether a stored media type may be served
// with Content-Disposition: inline.
func CardAssetServesInline(mediaType string) bool { return cardAssetRasterTypes[mediaType] }

// MaxCardAssetPixels caps one side of a raster upload. 12000 px is about three
// times an A4 sheet at 300 DPI, so no real artwork reaches it, and it bounds
// what a decoder would allocate for the picture (security review, #8634
// c53354 item 2). The size itself is read from the header only
// (image.DecodeConfig), never by decoding the pixels.
const MaxCardAssetPixels = 12000

// svgRootPattern matches a document whose first element is <svg ...>, after an
// optional BOM, XML declaration, comments and DOCTYPE. http.DetectContentType
// reports SVG as text/xml or text/plain, so the root element is the test.
var svgRootPattern = regexp.MustCompile(`(?is)^\xEF?\xBB?\xBF?\s*(<\?xml[^>]*\?>\s*)?(<!--.*?-->\s*)*(<!DOCTYPE[^>]*>\s*)?<svg[\s>]`)

// svgForbidden lists what an SVG in the store may never carry. The viewer only
// ever renders SVG through <img>, where none of this runs, but a blob: URL
// carries the app's origin, so an SVG that reached a document context would
// run as the admin; refusing at upload keeps the store free of such files
// rather than relying on every future consumer (security review, #8634
// c53354 item 1). Refuse, never sanitise-and-store.
// Element patterns tolerate a namespace prefix (`<s:script>` with xmlns:s
// bound to SVG is a script element: the namespace decides, not the prefix),
// and the XHTML namespace is refused outright, since an SVG asset has no use
// for it and it is the other way to spell a script element. An animation that
// targets a link (`<animate attributeName="href" values="javascript:...">`)
// rewrites a static `#fragment` at run time, so any animated href is refused,
// and `javascript:` anywhere is a blunt second net (security review of PR 139,
// #8642 c53387).
var (
	svgScript         = regexp.MustCompile(`(?i)<\s*(?:[a-z_][\w.-]*:)?script[\s>/]`)
	svgForeignObject  = regexp.MustCompile(`(?i)<\s*(?:[a-z_][\w.-]*:)?foreignObject[\s>/]`)
	svgEventAttr      = regexp.MustCompile(`(?i)[\s"'/]on[a-z]+\s*=`)
	svgXHTMLNamespace = regexp.MustCompile(`(?i)http://www\.w3\.org/1999/xhtml`)
	svgAnimatedHref   = regexp.MustCompile(`(?i)attributeName\s*=\s*["']\s*(?:[\w.-]+:)?href\s*["']`)
	svgJavascriptURL  = regexp.MustCompile(`(?i)javascript\s*:`)
	// Every href / xlink:href value; each must be a same-document #fragment
	// or a data:image/ URI.
	svgHref = regexp.MustCompile(`(?i)(?:[\w.-]+:)?href\s*=\s*["']([^"']*)["']`)
)

// ValidateSVGContent reports why an SVG upload is refused, or "" when it is
// acceptable. It is a textual check over the whole document (an SVG in this
// store is at most MaxCardAssetBytes), deliberately broader than an XML parse:
// a token that merely looks like a script element or an event attribute is
// enough to refuse, because the cost of a false refusal is one re-export from
// the drawing tool and the cost of a miss is script running as the admin.
func ValidateSVGContent(content []byte) string {
	switch {
	case svgScript.Match(content):
		return "SVG carries a <script> element"
	case svgForeignObject.Match(content):
		return "SVG carries a <foreignObject> element"
	case svgEventAttr.Match(content):
		return "SVG carries an on* event attribute"
	case svgXHTMLNamespace.Match(content):
		return "SVG names the XHTML namespace"
	case svgAnimatedHref.Match(content):
		return "SVG animates an href"
	case svgJavascriptURL.Match(content):
		return "SVG carries a javascript: URL"
	}
	for _, m := range svgHref.FindAllSubmatch(content, -1) {
		v := strings.ToLower(strings.TrimSpace(string(m[1])))
		if strings.HasPrefix(v, "#") || strings.HasPrefix(v, "data:image/") {
			continue
		}
		return "SVG carries an href that is not a #fragment or a data:image/ URI"
	}
	return ""
}

// DetectCardAssetType sniffs the media type and kind of an upload from its
// leading bytes (the first 512 are enough for every format here). The
// client's filename and Content-Type are never consulted. ok is false for any
// content outside the allowlist: raster images (PNG, JPEG, WebP, GIF), SVG, PDF
// and the four font containers (TTF, OTF, WOFF, WOFF2).
func DetectCardAssetType(head []byte) (mediaType, kind string, ok bool) {
	if len(head) >= 4 {
		switch {
		case bytes.Equal(head[:4], []byte{0x00, 0x01, 0x00, 0x00}), bytes.Equal(head[:4], []byte("true")):
			return "font/ttf", CardAssetKindFont, true
		case bytes.Equal(head[:4], []byte("OTTO")):
			return "font/otf", CardAssetKindFont, true
		case bytes.Equal(head[:4], []byte("wOFF")):
			return "font/woff", CardAssetKindFont, true
		case bytes.Equal(head[:4], []byte("wOF2")):
			return "font/woff2", CardAssetKindFont, true
		}
	}
	if svgRootPattern.Match(head) {
		return "image/svg+xml", CardAssetKindSVG, true
	}
	switch ct := http.DetectContentType(head); ct {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
		return ct, CardAssetKindImage, true
	case "application/pdf":
		return ct, CardAssetKindPDF, true
	}
	return "", "", false
}

// CardAssetPixelSize returns the pixel dimensions of a raster upload from its
// header alone (image.DecodeConfig never decodes pixels). WebP has no
// standard-library decoder, so it reports 0, 0 with no error; for PNG, JPEG
// and GIF a header that does not parse is an error, and the caller refuses
// the upload rather than store a picture it cannot measure. The caller also
// refuses a side over MaxCardAssetPixels.
func CardAssetPixelSize(mediaType string, content []byte) (w, h int, err error) {
	if mediaType == "image/webp" {
		return 0, 0, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}
