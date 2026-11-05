package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	imgTagRe            = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	uriObjectBlockRe    = regexp.MustCompile(`(?is)<URIObject\b([^>]*)>([\s\S]*?)</URIObject>`)
	origNameXMLRe       = regexp.MustCompile(`(?i)<OriginalName\b[^>]*\bv=(?:"([^"]*)"|'([^']*)')`)
	titleXMLRe          = regexp.MustCompile(`(?i)<Title>([^<]*)</Title>`)
	amsObjectPathRe     = regexp.MustCompile(`(?i)/v1/objects/([^/?#]+)`)
	amsObjectBareRe     = regexp.MustCompile(`(?i)/v1/objects/[^/?#]+$`)
	amsIDFormatRe       = regexp.MustCompile(`(?i)^0-[a-z0-9-]+$`)
	emojiItemTypeRe     = regexp.MustCompile(`(?i)itemtype=(?:"|')http://schema\.skype\.com/Emoji(?:"|')`)
	amsImageItemTypeRe  = regexp.MustCompile(`(?i)itemtype=(?:"|')http://schema\.skype\.com/AMSImage(?:"|')`)
	richTextImageTidRe  = regexp.MustCompile(`(?i)data-tid=(?:"|')rich-text-ima-image(?:"|')`)
	emojiClassAttrRe    = regexp.MustCompile(`(?i)\bclass=(?:"|')[^"']*(?:emoticon|emoji)[^"']*(?:"|')`)
	itemidAttrPresentRe = regexp.MustCompile(`(?i)\bitemid=(?:"|')[^"']+(?:"|')`)
	utf8FilenameDispRe  = regexp.MustCompile(`(?i)filename\*\s*=\s*UTF-8''([^;\r\n]+)`)
	plainFilenameDispRe = regexp.MustCompile(`(?i)filename\s*=\s*(?:"([^"]+)"|([^;\r\n]+))`)
)

var mimeExtMap = map[string]string{
	"image/png":                    "png",
	"image/jpeg":                   extJPG,
	"image/jpg":                    extJPG,
	"image/gif":                    extGIF,
	"image/webp":                   "webp",
	"image/svg+xml":                "svg",
	"image/bmp":                    "bmp",
	"image/x-icon":                 "ico",
	"image/vnd.microsoft.icon":     "ico",
	"image/tiff":                   "tiff",
	"image/heic":                   "heic",
	"image/avif":                   "avif",
	"application/pdf":              "pdf",
	"application/zip":              extZIP,
	"application/x-zip-compressed": extZIP,
	"application/gzip":             "gz",
	"application/x-tar":            "tar",
	"application/x-7z-compressed":  "7z",
	"application/x-rar-compressed": "rar",
	"application/json":             "json",
	"application/xml":              "xml",
	"text/xml":                     "xml",
	"text/plain":                   "txt",
	"text/csv":                     "csv",
	"text/markdown":                "md",
	"text/html":                    "html",
	"video/mp4":                    "mp4",
	"video/webm":                   "webm",
	"video/quicktime":              "mov",
	"audio/mpeg":                   "mp3",
	"audio/mp4":                    "m4a",
	"audio/wav":                    "wav",
	"audio/ogg":                    "ogg",
	"audio/webm":                   "weba",
	"application/msword":           "doc",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
	"application/vnd.ms-excel": "xls",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         "xlsx",
	"application/vnd.ms-powerpoint":                                             "ppt",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": "pptx",
}

type AttachmentDescriptor struct {
	Key               string `json:"key"`
	URL               string `json:"url"`
	ShareURL          string `json:"shareUrl"`
	SiteURL           string `json:"siteUrl"`
	ServerRelativeURL string `json:"serverRelativeUrl"`
	Filename          string `json:"filename"`
	Extension         string `json:"extension"`
	DataBase64        string `json:"dataBase64"`
	IsInlineImage     bool   `json:"isInlineImage"`
}

func isAMSOrMediaURL(u string) bool {
	return amsObjectPathRe.MatchString(u) ||
		strings.Contains(u, "asm.skype.com") ||
		strings.Contains(u, "asyncgw.teams.microsoft.com")
}

func extractAMSObjectID(u, fallbackID string) string {
	if m := amsObjectPathRe.FindStringSubmatch(u); len(m) > 1 {
		return m[1]
	}

	if amsIDFormatRe.MatchString(fallbackID) {
		return fallbackID
	}

	return ""
}

func isEmojiOrAvatarURL(u string) bool {
	lower := strings.ToLower(u)

	return strings.Contains(lower, "/emoticons/") ||
		strings.Contains(lower, "/pes/") ||
		strings.Contains(lower, "personal-expressions") ||
		strings.Contains(lower, "statics.teams.cdn.office.net") ||
		strings.Contains(lower, "profilepicture") ||
		strings.Contains(lower, "8:orgid:")
}

func isHTMLContentImageTag(tag string) bool {
	if emojiItemTypeRe.MatchString(tag) || emojiClassAttrRe.MatchString(tag) {
		return false
	}

	if amsImageItemTypeRe.MatchString(tag) || richTextImageTidRe.MatchString(tag) {
		return true
	}

	src := getHTMLAttr(tag, "data-orig-src")
	if src == "" {
		src = getHTMLAttr(tag, "src")
	}

	if src == "" || isEmojiOrAvatarURL(src) {
		return false
	}

	if isAMSOrMediaURL(src) {
		return true
	}

	if itemidAttrPresentRe.MatchString(tag) {
		return false
	}

	return strings.HasPrefix(src, "data:image/")
}

func getHTMLAttr(tag, attrName string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(attrName) + `\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>` + "`" + `]+))`)
	m := re.FindStringSubmatch(tag)

	if len(m) == 0 {
		return ""
	}

	for _, sub := range m[1:] {
		if sub != "" {
			return strings.TrimSpace(DecodeEntities(sub))
		}
	}

	return ""
}

func extractFilenameExtension(filename string) string {
	if filename == "" {
		return ""
	}

	base, _, _ := strings.Cut(filename, "?")
	base, _, _ = strings.Cut(base, "#")

	return cleanExt(filepath.Ext(strings.TrimSpace(base)))
}

func sanitizeAttachmentFilename(raw, fallbackExt string) string {
	cleaned := strings.ReplaceAll(raw, "\r", " ")
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	cleaned = strings.ReplaceAll(cleaned, "\t", " ")
	cleaned = strings.ReplaceAll(cleaned, "\\", "/")
	cleaned = path.Base(cleaned)
	cleaned, _, _ = strings.Cut(cleaned, "?")
	cleaned, _, _ = strings.Cut(cleaned, "#")
	cleaned = strings.TrimSpace(cleaned)

	if cleaned == "." || cleaned == "/" {
		cleaned = ""
	}

	lower := strings.ToLower(cleaned)
	if cleaned == "" || lower == "image" || lower == "afbeelding" || lower == "picture" {
		if fallbackExt != "" {
			return "image." + fallbackExt
		}

		return ""
	}

	if fallbackExt != "" && extractFilenameExtension(cleaned) == "" {
		cleaned += "." + fallbackExt
	}

	return cleaned
}

func extractFilenameFromURL(rawURL string) string {
	if rawURL == "" || strings.HasPrefix(rawURL, "data:") || strings.HasPrefix(rawURL, "blob:") {
		return ""
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	unescaped, err := url.PathUnescape(parsed.Path)
	if err != nil {
		unescaped = parsed.Path
	}

	base := path.Base(unescaped)
	switch base {
	case "", ".", "/", "imgo", "original", "imgt1", "$value", "content":
		return ""
	default:
		return sanitizeAttachmentFilename(base, "")
	}
}

func ExtractChatSvcAttachments(htmlContent string, rawFiles any) []AttachmentDescriptor {
	var out []AttachmentDescriptor

	out = append(out, parseFilesProperty(rawFiles)...)
	out = append(out, parseHTMLImageAttachments(htmlContent)...)
	out = append(out, parseURIObjectAttachments(htmlContent)...)

	return mergeAttachmentDescriptors(out)
}

func parseFilesProperty(rawFiles any) []AttachmentDescriptor {
	if rawFiles == nil {
		return nil
	}

	var data []byte

	switch v := rawFiles.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}

		data = []byte(v)
	case json.RawMessage:
		data = v
	default:
		var err error

		data, err = json.Marshal(v)
		if err != nil {
			return nil
		}
	}

	var items []map[string]any

	if err := json.Unmarshal(data, &items); err != nil {
		return nil
	}

	var out []AttachmentDescriptor

	for _, f := range items {
		if desc := buildFileObjectAttachment(f); desc != nil {
			out = append(out, *desc)
		}
	}

	return out
}

func buildFileObjectAttachment(f map[string]any) *AttachmentDescriptor {
	fileInfo, _ := f["fileInfo"].(map[string]any)
	botProps, _ := f["botFileProperties"].(map[string]any)

	fileURL := firstNonEmptyString(
		mapStr(fileInfo, "fileUrl"),
		mapStr(f, "fileUrl"),
		mapStr(f, "objectUrl"),
		mapStr(botProps, "url"),
		mapStr(f, "contentUrl"),
		mapStr(f, "downloadUrl"),
		mapStr(fileInfo, "shareUrl"),
	)
	shareURL := firstNonEmptyString(mapStr(fileInfo, "shareUrl"), mapStr(f, "shareUrl"))
	siteURL := firstNonEmptyString(mapStr(fileInfo, "siteUrl"), mapStr(f, "baseUrl"))
	serverRelURL := mapStr(fileInfo, "serverRelativeUrl")

	if fileURL == "" && shareURL == "" && (siteURL == "" || serverRelURL == "") {
		return nil
	}

	rawName := firstNonEmptyString(
		mapStr(f, "fileName"),
		mapStr(f, "title"),
		mapStr(f, "name"),
		mapStr(fileInfo, "fileName"),
		extractFilenameFromURL(firstNonEmptyString(fileURL, shareURL)),
	)
	ext := cleanExt(firstNonEmptyString(
		mapStr(f, "fileType"),
		mapStr(f, "type"),
		mapStr(fileInfo, "fileType"),
		extractFilenameExtension(rawName),
		extractFilenameExtension(fileURL),
	))
	key := firstNonEmptyString(
		mapStr(f, "itemid"),
		mapStr(f, "id"),
		mapStr(fileInfo, "itemId"),
		fileURL,
		shareURL,
		siteURL+":"+serverRelURL,
	)

	return &AttachmentDescriptor{ //nolint:exhaustruct_v5
		Key:               "file:" + key,
		URL:               firstNonEmptyString(fileURL, shareURL),
		ShareURL:          shareURL,
		SiteURL:           siteURL,
		ServerRelativeURL: serverRelURL,
		Filename:          sanitizeAttachmentFilename(rawName, ext),
		Extension:         ext,
	}
}

func parseHTMLImageAttachments(html string) []AttachmentDescriptor {
	if html == "" {
		return nil
	}

	var out []AttachmentDescriptor

	for _, tag := range imgTagRe.FindAllString(html, -1) {
		if !isHTMLContentImageTag(tag) {
			continue
		}

		u := firstNonEmptyString(getHTMLAttr(tag, "data-orig-src"), getHTMLAttr(tag, "src"))
		if u == "" || strings.HasPrefix(u, "blob:") {
			continue
		}

		amsID := extractAMSObjectID(u, firstNonEmptyString(getHTMLAttr(tag, "id"), getHTMLAttr(tag, "itemid")))
		rawName := firstNonEmptyString(getHTMLAttr(tag, "data-file-name"), getHTMLAttr(tag, "title"), getHTMLAttr(tag, "alt"))
		ext := extractFilenameExtension(rawName)
		key := "img:" + u

		if amsID != "" {
			key = "ams:" + amsID
		}

		out = append(out, AttachmentDescriptor{ //nolint:exhaustruct_v5
			Key:           key,
			URL:           u,
			Filename:      sanitizeAttachmentFilename(rawName, ext),
			Extension:     ext,
			IsInlineImage: true,
		})
	}

	return out
}

func parseURIObjectAttachments(html string) []AttachmentDescriptor {
	if html == "" {
		return nil
	}

	var out []AttachmentDescriptor

	for _, m := range uriObjectBlockRe.FindAllStringSubmatch(html, -1) {
		attrs := m[1]
		inner := m[2]
		tag := "<URIObject " + attrs + ">"
		uri := getHTMLAttr(tag, "uri")
		thumb := getHTMLAttr(tag, "url_thumbnail")
		objType := strings.ToLower(getHTMLAttr(tag, "type"))

		var rawName string

		if om := origNameXMLRe.FindStringSubmatch(inner); len(om) > 1 {
			rawName = firstNonEmptyString(om[1], om[2])
		} else if tm := titleXMLRe.FindStringSubmatch(inner); len(tm) > 1 {
			rawName = tm[1]
		}

		rawName = strings.TrimSpace(DecodeEntities(rawName))
		targetURL := resolveURIObjectTargetURL(uri, thumb, objType)

		if targetURL == "" {
			continue
		}

		amsID := extractAMSObjectID(targetURL, getHTMLAttr(tag, "doc_id"))
		ext := extractFilenameExtension(rawName)
		key := "uri:" + targetURL

		if amsID != "" {
			key = "ams:" + amsID
		}

		out = append(out, AttachmentDescriptor{ //nolint:exhaustruct_v5
			Key:           key,
			URL:           targetURL,
			Filename:      sanitizeAttachmentFilename(rawName, ext),
			Extension:     ext,
			IsInlineImage: strings.HasPrefix(objType, "picture"),
		})
	}

	return out
}

func resolveURIObjectTargetURL(uri, thumb, objType string) string {
	targetURL := firstNonEmptyString(uri, thumb)
	if uri == "" || !amsObjectBareRe.MatchString(uri) {
		return targetURL
	}

	switch {
	case strings.HasPrefix(objType, "picture"):
		return uri + "/views/imgo"
	case strings.HasPrefix(objType, "video"):
		return uri + "/views/video"
	case strings.HasPrefix(objType, "audio"):
		return uri + "/views/audio"
	default:
		return uri + "/views/original"
	}
}

func mergeAttachmentDescriptors(list []AttachmentDescriptor) []AttachmentDescriptor {
	var keys []string

	byKey := make(map[string]AttachmentDescriptor, len(list))

	for _, item := range list {
		if item.Key == "" {
			continue
		}

		prev, exists := byKey[item.Key]
		if !exists {
			keys = append(keys, item.Key)
			byKey[item.Key] = item

			continue
		}

		filename := prev.Filename
		if item.Filename != "" && (!strings.HasPrefix(item.Filename, "image.") || filename == "") {
			filename = item.Filename
		}

		byKey[item.Key] = AttachmentDescriptor{
			Key:               item.Key,
			URL:               firstNonEmptyString(item.URL, prev.URL),
			ShareURL:          firstNonEmptyString(item.ShareURL, prev.ShareURL),
			SiteURL:           firstNonEmptyString(item.SiteURL, prev.SiteURL),
			ServerRelativeURL: firstNonEmptyString(item.ServerRelativeURL, prev.ServerRelativeURL),
			Filename:          filename,
			Extension:         firstNonEmptyString(item.Extension, prev.Extension),
			DataBase64:        firstNonEmptyString(item.DataBase64, prev.DataBase64),
			IsInlineImage:     item.IsInlineImage || prev.IsInlineImage,
		}
	}

	out := make([]AttachmentDescriptor, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}

	return out
}

var magicPrefixes = []struct {
	prefix []byte
	ext    string
}{
	{[]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, "png"},
	{[]byte{0xff, 0xd8, 0xff}, extJPG},
	{[]byte("GIF87a"), extGIF},
	{[]byte("GIF89a"), extGIF},
	{[]byte("%PDF-"), "pdf"},
	{[]byte{0x50, 0x4b, 0x03, 0x04}, extZIP},
	{[]byte{0x42, 0x4d}, "bmp"},
	{[]byte{0x1f, 0x8b}, "gz"},
}

func DetectBufferExtension(buf []byte) string {
	for _, m := range magicPrefixes {
		if bytes.HasPrefix(buf, m.prefix) {
			return m.ext
		}
	}

	if len(buf) >= 12 {
		if bytes.Equal(buf[:4], []byte("RIFF")) && bytes.Equal(buf[8:12], []byte("WEBP")) {
			return "webp"
		}

		if bytes.Equal(buf[4:8], []byte("ftyp")) {
			return "mp4"
		}
	}

	return ""
}

func ExtensionFromContentType(contentType string) string {
	mime, _, _ := strings.Cut(contentType, ";")
	return mimeExtMap[strings.ToLower(strings.TrimSpace(mime))]
}

func parseContentDispositionFilename(header string) string {
	if header == "" {
		return ""
	}

	if m := utf8FilenameDispRe.FindStringSubmatch(header); len(m) > 1 {
		raw := strings.TrimSpace(m[1])
		if unescaped, err := url.PathUnescape(raw); err == nil {
			return unescaped
		}

		return raw
	}

	if m := plainFilenameDispRe.FindStringSubmatch(header); len(m) > 1 {
		return strings.TrimSpace(firstNonEmptyString(m[1], m[2]))
	}

	return ""
}

func encodeShareURLToken(targetURL string) string {
	return "u!" + base64.RawURLEncoding.EncodeToString([]byte(targetURL))
}

type downloadedBlob struct {
	buffer          []byte
	contentType     string
	dispositionName string
}

func (w *Worker) resolveAndSaveAttachment(ctx context.Context, messageID int64, desc AttachmentDescriptor) *AttachmentPayload {
	expectedExt := cleanExt(firstNonEmptyString(desc.Extension, extractFilenameExtension(desc.Filename)))
	blob := w.fetchAttachmentBlob(ctx, desc, expectedExt)

	if blob == nil || len(blob.buffer) == 0 || isSpacerGIF(blob.buffer) {
		return nil
	}

	rawName := desc.Filename
	if rawName == "" || strings.HasPrefix(rawName, "image.") {
		rawName = firstNonEmptyString(blob.dispositionName, desc.Filename)
	}

	ext := firstNonEmptyString(
		extractFilenameExtension(rawName),
		expectedExt,
		DetectBufferExtension(blob.buffer),
		ExtensionFromContentType(blob.contentType),
		"bin",
	)

	filename := sanitizeAttachmentFilename(rawName, ext)
	if filename == "" {
		if desc.IsInlineImage {
			filename = "image." + ext
		} else {
			filename = "attachment." + ext
		}
	}

	checksum, savedExt, err := w.SaveAttachment(blob.buffer, ext)
	if err != nil || checksum == "" {
		return nil
	}

	return &AttachmentPayload{ //nolint:exhaustruct_v5
		MessageID: messageID,
		Filename:  filename,
		Checksum:  checksum,
		Extension: savedExt,
	}
}

func isSpacerGIF(buf []byte) bool {
	if len(buf) < 10 || !bytes.Equal(buf[:4], []byte("GIF8")) {
		return false
	}

	w := int(buf[6]) | int(buf[7])<<8
	h := int(buf[8]) | int(buf[9])<<8

	return w <= 1 && h <= 1
}

func (w *Worker) fetchAttachmentBlob(ctx context.Context, desc AttachmentDescriptor, expectedExt string) *downloadedBlob {
	if desc.DataBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(desc.DataBase64)
		if err == nil && len(raw) > 0 {
			return &downloadedBlob{buffer: raw, contentType: "", dispositionName: ""}
		}
	}

	if strings.HasPrefix(desc.URL, "data:") {
		if blob := decodeDataURL(desc.URL); blob != nil {
			return blob
		}
	}

	if desc.URL == "" && desc.ShareURL == "" && (desc.SiteURL == "" || desc.ServerRelativeURL == "") {
		return nil
	}

	if isAMSOrMediaURL(desc.URL) {
		return w.downloadAMSBlob(ctx, desc.URL, expectedExt)
	}

	return w.downloadSharePointOrDirectBlob(ctx, desc, expectedExt)
}

func decodeDataURL(dataURL string) *downloadedBlob {
	header, payload, ok := strings.Cut(strings.TrimPrefix(dataURL, "data:"), ",")
	if !ok {
		return nil
	}

	contentType, _, _ := strings.Cut(header, ";")
	if strings.HasSuffix(header, ";base64") {
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err == nil && len(raw) > 0 {
			return &downloadedBlob{buffer: raw, contentType: contentType, dispositionName: ""}
		}
	}

	return nil
}

func (w *Worker) downloadAMSBlob(ctx context.Context, targetURL, expectedExt string) *downloadedBlob {
	candidates := []string{targetURL}
	if strings.HasSuffix(strings.ToLower(targetURL), "/views/imgo") {
		base := targetURL[:len(targetURL)-len("/views/imgo")]
		candidates = append(candidates,
			base+"/views/imgpsh_fullsize_anim",
			base+"/views/imgpsh_fullsize",
			base+"/views/original",
		)
	} else if strings.HasSuffix(strings.ToLower(targetURL), "/views/original") {
		base := targetURL[:len(targetURL)-len("/views/original")]
		candidates = append(candidates, base+"/views/imgo")
	}

	tokens := w.currentTokens()

	for _, cand := range candidates {
		for _, tok := range []string{tokens.IC3Token, tokens.SpacesToken, tokens.SkypeToken} {
			if tok == "" {
				continue
			}

			headers := map[string]string{
				headerAuthorization: "Bearer " + tok,
				"x-ms-migration":    "True",
			}
			if blob := w.httpGetBlob(ctx, cand, headers, expectedExt); blob != nil {
				return blob
			}
		}

		if tokens.SkypeToken != "" {
			headers := map[string]string{
				headerAuthorization: "skype_token " + tokens.SkypeToken,
			}
			if blob := w.httpGetBlob(ctx, cand, headers, expectedExt); blob != nil {
				return blob
			}
		}
	}

	return nil
}

func (w *Worker) downloadSharePointOrDirectBlob(ctx context.Context, desc AttachmentDescriptor, expectedExt string) *downloadedBlob {
	primaryURL := firstNonEmptyString(desc.URL, desc.ShareURL)
	tokens := w.currentTokens()

	var origin string

	if parsed, err := url.Parse(firstNonEmptyString(primaryURL, desc.SiteURL)); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		origin = parsed.Scheme + "://" + parsed.Host
	}

	if strings.Contains(origin, ".sharepoint.com") {
		if blob := w.downloadSharePointBlob(ctx, origin, primaryURL, desc, tokens, expectedExt); blob != nil {
			return blob
		}
	}

	if primaryURL == "" {
		return nil
	}

	for _, tok := range []string{tokens.IC3Token, tokens.SpacesToken} {
		if tok == "" {
			continue
		}

		if blob := w.httpGetBlob(ctx, primaryURL, map[string]string{headerAuthorization: "Bearer " + tok}, expectedExt); blob != nil {
			return blob
		}
	}

	return nil
}

func (w *Worker) downloadSharePointBlob(ctx context.Context, origin, primaryURL string, desc AttachmentDescriptor, tokens AuthTokens, expectedExt string) *downloadedBlob {
	spToken := tokens.SharePointTokens[origin]
	spHeaders := map[string]string{}

	if spToken != "" {
		spHeaders[headerAuthorization] = "Bearer " + spToken
	}

	urls := []string{desc.URL, desc.ShareURL}
	if blob := w.tryShareEndpoint(ctx, origin+"/_api/v2.0/shares", spHeaders, urls, expectedExt); blob != nil {
		return blob
	}

	if desc.SiteURL != "" && desc.ServerRelativeURL != "" {
		siteBase := strings.TrimRight(desc.SiteURL, "/")
		relPath := strings.ReplaceAll(desc.ServerRelativeURL, "'", "''")
		apiURL := siteBase + "/_api/web/GetFileByServerRelativePath(decodedurl='" + url.PathEscape(relPath) + "')/$value"

		if blob := w.httpGetBlob(ctx, apiURL, spHeaders, expectedExt); blob != nil {
			return blob
		}
	}

	if tokens.GraphToken != "" {
		graphHeaders := map[string]string{headerAuthorization: "Bearer " + tokens.GraphToken}
		if blob := w.tryShareEndpoint(ctx, "https://graph.microsoft.com/v1.0/shares", graphHeaders, urls, expectedExt); blob != nil {
			return blob
		}
	}

	if primaryURL != "" && spToken != "" {
		return w.httpGetBlob(ctx, primaryURL, spHeaders, expectedExt)
	}

	return nil
}

func (w *Worker) tryShareEndpoint(ctx context.Context, sharesBase string, headers map[string]string, urls []string, expectedExt string) *downloadedBlob {
	for _, u := range urls {
		if u == "" {
			continue
		}

		apiURL := sharesBase + "/" + encodeShareURLToken(u) + "/driveItem/content"
		if blob := w.httpGetBlob(ctx, apiURL, headers, expectedExt); blob != nil {
			return blob
		}
	}

	return nil
}

func (w *Worker) httpGetBlob(ctx context.Context, targetURL string, headers map[string]string, expectedExt string) *downloadedBlob {
	client := &http.Client{Timeout: 30 * time.Second} //nolint:exhaustruct_v5

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	contentType := resp.Header.Get("Content-Type")
	mime, _, _ := strings.Cut(strings.ToLower(contentType), ";")

	if strings.TrimSpace(mime) == "text/html" && expectedExt != "html" && expectedExt != "htm" {
		return nil
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil || len(data) == 0 {
		return nil
	}

	return &downloadedBlob{
		buffer:          data,
		contentType:     contentType,
		dispositionName: parseContentDispositionFilename(resp.Header.Get("Content-Disposition")),
	}
}

func (w *Worker) hasArchivedAttachments(messageID int64, minCount int) bool {
	if minCount <= 0 {
		return true
	}

	var rows []InitAttachment

	err := w.db.Raw(`
		SELECT message_id, COALESCE(filename, '') AS filename, COALESCE(checksum, '') AS checksum, COALESCE(extension, '') AS extension
		FROM message_attachments
		WHERE message_id = ?
	`, messageID).Scan(&rows).Error
	if err != nil || len(rows) < minCount {
		return false
	}

	for _, r := range rows {
		if len(r.Checksum) < 6 {
			return false
		}

		name := r.Checksum
		if ext := cleanExt(r.Extension); ext != "" {
			name += "." + ext
		}

		p := filepath.Join(w.dir, "attachments", r.Checksum[0:2], r.Checksum[2:4], r.Checksum[4:6], name)
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}

	return true
}

func mapStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}

	if v, ok := m[key].(string); ok {
		return strings.TrimSpace(v)
	}

	return ""
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}

	return ""
}
