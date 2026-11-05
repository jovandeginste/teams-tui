package worker

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

var (
	preTagRe          = regexp.MustCompile(`(?is)<pre\b([^>]*)>([\s\S]*?)</pre>`)
	codeTagRe         = regexp.MustCompile(`(?is)<code\b[^>]*>([\s\S]*?)</code>`)
	blockquoteTagRe   = regexp.MustCompile(`(?is)<blockquote\b[^>]*>([\s\S]*?)</blockquote>`)
	strongMriRe       = regexp.MustCompile(`(?is)<strong\b[^>]*itemprop="mri"[^>]*>([\s\S]*?)</strong>`)
	spanTimeRe        = regexp.MustCompile(`(?is)<span\b[^>]*itemprop="time"[^>]*>([\s\S]*?)</span>`)
	pPreviewRe        = regexp.MustCompile(`(?is)<p\b[^>]*itemprop="preview"[^>]*>([\s\S]*?)</p>`)
	uriObjectRe       = regexp.MustCompile(`(?is)<URIObject\b[\s\S]*?</URIObject>`)
	imgOrEmojiTagRe   = regexp.MustCompile(`(?is)<(?:img|emoji)\b[^>]*>`)
	readonlyMentionRe = regexp.MustCompile(`(?is)<readonly\b[^>]*itemtype="http://schema\.skype\.com/Mention"[^>]*>([\s\S]*?)</readonly>`)
	spanMentionRe     = regexp.MustCompile(`(?is)<span\b[^>]*itemtype="http://schema\.skype\.com/Mention"[^>]*>([\s\S]*?)</span>`)
	brTagRe           = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockEndTagRe     = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6]|blockquote|pre)>`)
	anyTagRe          = regexp.MustCompile(`<[^>]+>`)
	entityRe          = regexp.MustCompile(`(?i)&(#x?[0-9a-f]+|[a-z]+);`)
	codeLangRe        = regexp.MustCompile(`(?i)\blanguage-([a-z0-9_+-]+)`)
	codeTokenRe       = regexp.MustCompile("\x00CODE_(\\d+)\x00")
	trailingSpaceRe   = regexp.MustCompile(`[ \t]+\n`)
	multiNewlineRe    = regexp.MustCompile(`\n{3,}`)
	hexEmojiPrefixRe  = regexp.MustCompile(`(?i)^([0-9a-f]{4,6})_`)

	callStartedXMLRe   = regexp.MustCompile(`(?i)<callEventType>\s*callStarted\s*</callEventType>|<partlist\b[^>]*type="callStarted"`)
	callEndedXMLRe     = regexp.MustCompile(`(?i)<callEventType>\s*callEnded\s*</callEventType>|<partlist\b`)
	organizerXMLRe     = regexp.MustCompile(`(?i)<Organizer>([^<]+)</Organizer>`)
	partBlockXMLRe     = regexp.MustCompile(`(?is)<part\b[^>]*>([\s\S]*?)</part>`)
	nameXMLRe          = regexp.MustCompile(`(?i)<name>([^<]+)</name>`)
	displayNameXMLRe   = regexp.MustCompile(`(?i)<displayName>([^<]+)</displayName>`)
	durationXMLRe      = regexp.MustCompile(`(?i)<duration>(\d+)</duration>`)
	memberUpdateXMLRe  = regexp.MustCompile(`(?i)<(?:addmember|deletemember|roleupdate)\b`)
	deleteMemberXMLRe  = regexp.MustCompile(`(?i)<deletemember\b`)
	roleUpdateXMLRe    = regexp.MustCompile(`(?i)<roleupdate\b`)
	initiatorXMLRe     = regexp.MustCompile(`(?i)<initiator>([^<]+)</initiator>`)
	targetXMLRe        = regexp.MustCompile(`(?i)<target>([^<]+)</target>`)
	topicUpdateXMLRe   = regexp.MustCompile(`(?i)<topicupdate\b`)
	valueXMLRe         = regexp.MustCompile(`(?is)<value>([\s\S]*?)</value>`)
	callStartedTextRe  = regexp.MustCompile(`(?i)(?:^|false|true|Adhoc|Scheduled|Recurring|Occurrence)callStarted\b`)
	callOrgEmailRe     = regexp.MustCompile(`(?i)([A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,})(?:Scheduled|Recurring|Adhoc)`)
	thirteenDigitsRe   = regexp.MustCompile(`\d{13}`)
	mriTokenRe         = regexp.MustCompile(`(?i)(8:orgid:[0-9a-f-]{36}|8:teamsvisitor:[0-9a-f]{32}|28:app:[0-9a-f_-]+|28:[0-9a-f-]{36})`)
	partlistEntryRe    = regexp.MustCompile(`(?i)(8:orgid:[0-9a-f-]{36}|8:live:\.cid\.[0-9a-f]+|8:live:[a-z0-9_.-]+|8:teamsvisitor:[0-9a-f]{32}|28:(?:app:)?[0-9a-f_-]{36,73})([^0-9\n]*?)(\d+)`)
	callLegacyPrefixRe = regexp.MustCompile(`^(?:8:orgid:|8:live:|8:teamsvisitor:|28:|\d{13}(?:8:orgid:|19:))`)
)

var htmlEntities = map[string]string{
	"amp":  "&",
	"lt":   "<",
	"gt":   ">",
	"quot": `"`,
	"apos": "'",
	"nbsp": " ",
}

var skypeEmojiMap = map[string]string{
	"like":      "👍",
	"heart":     "❤️",
	"laugh":     "😆",
	"surprised": "😮",
	"sad":       "😢",
	"angry":     "😠",
	"ok":        "👌",
	"think":     "🤔",
	"yes":       "👍",
}

func EmojiFromItemID(itemID string) string {
	if itemID == "" {
		return ""
	}

	if em, ok := skypeEmojiMap[itemID]; ok {
		return em
	}

	if m := hexEmojiPrefixRe.FindStringSubmatch(itemID); len(m) > 1 {
		if cp, err := strconv.ParseInt(m[1], 16, 32); err == nil && cp > 0 {
			return string(rune(cp))
		}
	}

	return ":" + itemID + ":"
}

func DecodeEntities(str string) string {
	return entityRe.ReplaceAllStringFunc(str, func(match string) string {
		ent := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(match, "&"), ";"))
		if val, ok := htmlEntities[ent]; ok {
			return val
		}

		if after, ok := strings.CutPrefix(ent, "#x"); ok {
			if cp, err := strconv.ParseInt(after, 16, 32); err == nil && cp > 0 {
				return string(rune(cp))
			}
		}

		if after, ok := strings.CutPrefix(ent, "#"); ok {
			if cp, err := strconv.ParseInt(after, 10, 32); err == nil && cp > 0 {
				return string(rune(cp))
			}
		}

		return match
	})
}

func normalizeCodeLang(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	switch lower {
	case "plaintext", "plain text", "none", "text":
		return ""
	default:
		return lower
	}
}

func formatQuoteBlock(author, timeStr, preview string) string {
	var lines []string

	if author != "" {
		if timeStr != "" {
			lines = append(lines, fmt.Sprintf("> %s (%s):", author, timeStr))
		} else {
			lines = append(lines, fmt.Sprintf("> %s:", author))
		}
	}

	if preview != "" {
		for line := range strings.SplitSeq(preview, "\n") {
			lines = append(lines, "> "+line)
		}
	}

	if len(lines) == 0 {
		return ""
	}

	return strings.Join(lines, "\n") + "\n"
}

func HTMLToText(html string) string {
	var placeholders []string

	return htmlToTextWithPlaceholders(html, &placeholders)
}

func htmlToTextWithPlaceholders(html string, codePlaceholders *[]string) string {
	if html == "" {
		return ""
	}

	stashCode := func(text string) string {
		token := fmt.Sprintf("\x00CODE_%d\x00", len(*codePlaceholders))
		*codePlaceholders = append(*codePlaceholders, text)

		return token
	}

	s := strings.ReplaceAll(html, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = replacePreAndCodeTags(s, stashCode)
	s = replaceBlockquoteTags(s, codePlaceholders)
	s = uriObjectRe.ReplaceAllString(s, "")
	s = imgOrEmojiTagRe.ReplaceAllStringFunc(s, replaceImgOrEmojiTag)
	s = readonlyMentionRe.ReplaceAllString(s, "$1 ")
	s = spanMentionRe.ReplaceAllString(s, "$1 ")
	s = brTagRe.ReplaceAllString(s, "\n")
	s = blockEndTagRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, "")

	s = DecodeEntities(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = codeTokenRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := codeTokenRe.FindStringSubmatch(m)
		if len(sub) > 1 {
			if idx, err := strconv.Atoi(sub[1]); err == nil && idx >= 0 && idx < len(*codePlaceholders) {
				return (*codePlaceholders)[idx]
			}
		}

		return ""
	})
	s = trailingSpaceRe.ReplaceAllString(s, "\n")
	s = multiNewlineRe.ReplaceAllString(s, "\n\n")

	return strings.TrimSpace(s)
}

func replacePreAndCodeTags(s string, stash func(string) string) string {
	s = preTagRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := preTagRe.FindStringSubmatch(match)
		if len(sub) < 3 {
			return ""
		}

		var lang string

		if lm := codeLangRe.FindStringSubmatch(sub[1]); len(lm) > 1 {
			lang = normalizeCodeLang(lm[1])
		}

		inner := brTagRe.ReplaceAllString(sub[2], "\n")
		inner = blockEndTagRe.ReplaceAllString(inner, "\n")
		inner = anyTagRe.ReplaceAllString(inner, "")
		code := strings.TrimRight(strings.ReplaceAll(DecodeEntities(inner), "\u00a0", " "), "\n")

		return "\n" + stash(fmt.Sprintf("```%s\n%s\n```", lang, code)) + "\n"
	})

	return codeTagRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := codeTagRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return ""
		}

		inner := brTagRe.ReplaceAllString(sub[1], "\n")
		inner = anyTagRe.ReplaceAllString(inner, "")
		code := strings.ReplaceAll(DecodeEntities(inner), "\u00a0", " ")

		return stash("`" + code + "`")
	})
}

func replaceBlockquoteTags(s string, codePlaceholders *[]string) string {
	return blockquoteTagRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := blockquoteTagRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return ""
		}

		inner := sub[1]
		authorSub := strongMriRe.FindStringSubmatch(inner)
		timeSub := spanTimeRe.FindStringSubmatch(inner)
		previewSub := pPreviewRe.FindStringSubmatch(inner)

		if len(authorSub) > 1 || len(previewSub) > 1 {
			var authorRaw, timeRaw, previewRaw string

			if len(authorSub) > 1 {
				authorRaw = authorSub[1]
			}

			if len(timeSub) > 1 {
				timeRaw = timeSub[1]
			}

			if len(previewSub) > 1 {
				previewRaw = previewSub[1]
			}

			author := htmlToTextWithPlaceholders(authorRaw, codePlaceholders)
			timeStr := htmlToTextWithPlaceholders(timeRaw, codePlaceholders)
			preview := htmlToTextWithPlaceholders(previewRaw, codePlaceholders)

			return "\n" + formatQuoteBlock(author, timeStr, preview)
		}

		body := htmlToTextWithPlaceholders(inner, codePlaceholders)

		return "\n" + formatQuoteBlock("", "", body)
	})
}

func replaceImgOrEmojiTag(tag string) string {
	if strings.HasPrefix(strings.ToLower(tag), "<img") && isHTMLContentImageTag(tag) {
		return ""
	}

	if alt := getHTMLAttr(tag, "alt"); alt != "" {
		return alt
	}

	if id := getHTMLAttr(tag, "itemid"); id != "" {
		return EmojiFromItemID(id)
	}

	if id := getHTMLAttr(tag, "id"); id != "" {
		return EmojiFromItemID(id)
	}

	return ""
}

func FormatEventContent(tx *gorm.DB, rawContent, msgType string) string {
	text := strings.TrimSpace(rawContent)
	if text == "" {
		return ""
	}

	if strings.HasPrefix(text, "<") {
		if formatted, ok := formatXMLEvent(tx, text); ok {
			return formatted
		}
	}

	if strings.HasPrefix(text, "{") {
		if formatted, ok := formatJSONEvent(tx, text); ok {
			return formatted
		}
	}

	if callStartedTextRe.MatchString(text) {
		if m := callOrgEmailRe.FindStringSubmatch(text); len(m) > 1 {
			return fmt.Sprintf("Call started (%s)", m[1])
		}

		return "Call started"
	}

	if strings.Contains(text, "callEnded") || callLegacyPrefixRe.MatchString(text) {
		if formatted, ok := formatLegacyCallOrMemberEvent(tx, text); ok {
			return formatted
		}
	}

	if msgType != "" && !strings.HasPrefix(msgType, "RichText") && msgType != "Text" {
		return HTMLToText(text)
	}

	return text
}

func formatXMLEvent(tx *gorm.DB, text string) (string, bool) {
	if callStartedXMLRe.MatchString(text) {
		if m := organizerXMLRe.FindStringSubmatch(text); len(m) > 1 {
			if org := strings.TrimSpace(m[1]); org != "" {
				return fmt.Sprintf("Call started (%s)", org), true
			}
		}

		return "Call started", true
	}

	if callEndedXMLRe.MatchString(text) {
		return formatXMLCallEnded(tx, text), true
	}

	if memberUpdateXMLRe.MatchString(text) {
		if res := formatXMLMemberUpdate(tx, text); res != "" {
			return res, true
		}
	}

	if topicUpdateXMLRe.MatchString(text) {
		return formatXMLTopicUpdate(tx, text), true
	}

	return "", false
}

func formatXMLCallEnded(tx *gorm.DB, text string) string {
	var parts []string

	seen := make(map[string]bool)

	for _, pm := range partBlockXMLRe.FindAllStringSubmatch(text, -1) {
		inner := pm[1]
		rawID := firstSubmatch(nameXMLRe, inner)
		disp := firstSubmatch(displayNameXMLRe, inner)
		dur := parseSeconds(firstSubmatch(durationXMLRe, inner))

		normID := normalizeAuthorID(rawID)
		if tx != nil && normID != "" && disp != "" && !strings.HasPrefix(normID, "28:") && !strings.HasPrefix(normID, "4:") {
			if err := upsertAuthor(tx, normID, &disp); err != nil {
				continue
			}
		}

		name := resolvePersonName(tx, rawID, disp)
		if name == "" || name == personBot || name == personApp || seen[name] {
			continue
		}

		seen[name] = true

		if durStr := formatDurationSeconds(dur); durStr != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", name, durStr))
		} else {
			parts = append(parts, name)
		}
	}

	if len(parts) > 0 {
		return "Call ended — " + strings.Join(parts, ", ")
	}

	return "Call ended"
}

func formatXMLMemberUpdate(tx *gorm.DB, text string) string {
	isDelete := deleteMemberXMLRe.MatchString(text)
	action := "added"

	if isDelete {
		action = "removed"
	} else if roleUpdateXMLRe.MatchString(text) {
		action = "updated role for"
	}

	initiator := resolvePersonName(tx, firstSubmatch(initiatorXMLRe, text), "")

	var allTargets, targets []string

	for _, tm := range targetXMLRe.FindAllStringSubmatch(text, -1) {
		name := resolvePersonName(tx, strings.TrimSpace(tm[1]), "")
		if name == "" {
			continue
		}

		if !slices.Contains(allTargets, name) {
			allTargets = append(allTargets, name)
		}

		if name != initiator && !slices.Contains(targets, name) {
			targets = append(targets, name)
		}
	}

	if initiator != "" && len(targets) > 0 {
		return fmt.Sprintf("%s %s %s", initiator, action, strings.Join(targets, ", "))
	}

	if len(targets) > 0 {
		return fmt.Sprintf("Members %s: %s", action, strings.Join(targets, ", "))
	}

	if isDelete && initiator != "" && slices.Contains(allTargets, initiator) {
		return initiator + " left"
	}

	if initiator != "" && len(allTargets) > 0 {
		return fmt.Sprintf("%s %s %s", initiator, action, strings.Join(allTargets, ", "))
	}

	return ""
}

func formatXMLTopicUpdate(tx *gorm.DB, text string) string {
	initiator := resolvePersonName(tx, firstSubmatch(initiatorXMLRe, text), "")
	val := HTMLToText(firstSubmatch(valueXMLRe, text))

	if initiator != "" {
		return fmt.Sprintf(`%s changed topic to "%s"`, initiator, val)
	}

	return fmt.Sprintf(`Topic changed to "%s"`, val)
}

func formatJSONEvent(tx *gorm.DB, text string) (string, bool) {
	var obj struct {
		Initiator string  `json:"initiator"`
		User      string  `json:"user"`
		Topic     *string `json:"topic"`
		NewValue  any     `json:"newValue"`
		Members   []struct {
			ID           string `json:"id"`
			FriendlyName string `json:"friendlyname"`
		} `json:"members"`
	}

	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return "", false
	}

	if len(obj.Members) > 0 {
		initiator := resolvePersonName(tx, obj.Initiator, "")

		var members []string

		for _, m := range obj.Members {
			name := resolvePersonName(tx, m.ID, strings.TrimSpace(m.FriendlyName))
			if name != "" && name != initiator {
				members = append(members, name)
			}
		}

		if initiator != "" && len(members) > 0 {
			return fmt.Sprintf("%s added %s", initiator, strings.Join(members, ", ")), true
		}

		if len(members) > 0 {
			return "Added " + strings.Join(members, ", "), true
		}
	}

	if obj.Topic != nil {
		user := resolvePersonName(tx, obj.User, "")
		if user != "" {
			return fmt.Sprintf(`%s changed topic to "%s"`, user, *obj.Topic), true
		}

		return fmt.Sprintf(`Topic changed to "%s"`, *obj.Topic), true
	}

	if obj.NewValue != nil {
		return formatJSONNewValueEvent(tx, obj.User, fmt.Sprint(obj.NewValue)), true
	}

	return "", false
}

func formatJSONNewValueEvent(tx *gorm.DB, rawUser, rawSummary string) string {
	user := resolvePersonName(tx, rawUser, "")
	summary := strings.TrimSpace(rawSummary)

	if names := parseJSONChannelNames(summary); len(names) > 0 {
		if user != "" {
			return fmt.Sprintf("%s updated channels (%s)", user, strings.Join(names, ", "))
		}

		return fmt.Sprintf("Updated channels (%s)", strings.Join(names, ", "))
	}

	summary = strings.Join(strings.Fields(summary), " ")
	if user != "" {
		return fmt.Sprintf(`%s updated description: "%s"`, user, summary)
	}

	return fmt.Sprintf(`Updated description: "%s"`, summary)
}

func parseJSONChannelNames(summary string) []string {
	if !strings.HasPrefix(summary, "[") {
		return nil
	}

	var arr []struct {
		Name string `json:"name"`
	}

	if err := json.Unmarshal([]byte(summary), &arr); err != nil {
		return nil
	}

	var names []string

	for _, x := range arr {
		if x.Name != "" {
			names = append(names, x.Name)
		}
	}

	return names
}

func formatLegacyCallOrMemberEvent(tx *gorm.DB, text string) (string, bool) {
	if res, ok := formatLegacyMemberEvent(tx, text); ok {
		return res, true
	}

	var parts []string

	seen := make(map[string]bool)

	for _, m := range partlistEntryRe.FindAllStringSubmatch(text, -1) {
		rawID := m[1]
		if strings.HasPrefix(rawID, "28:") {
			continue
		}

		inlineName := strings.TrimSpace(m[2])
		dur := parseSeconds(m[3])
		name := resolvePersonName(tx, rawID, inlineName)

		if name == "" || seen[name] {
			continue
		}

		seen[name] = true

		if durStr := formatDurationSeconds(dur); durStr != "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", name, durStr))
		} else {
			parts = append(parts, name)
		}
	}

	if len(parts) > 0 || strings.Contains(text, "callEnded") {
		if len(parts) > 0 {
			return "Call ended — " + strings.Join(parts, ", "), true
		}

		return "Call ended", true
	}

	return "", false
}

func formatLegacyMemberEvent(tx *gorm.DB, text string) (string, bool) {
	withoutTS := thirteenDigitsRe.ReplaceAllString(text, " ")
	idMatches := mriTokenRe.FindAllString(withoutTS, -1)

	if !thirteenDigitsRe.MatchString(text) || strings.Contains(text, "callEnded") || len(idMatches) == 0 {
		return "", false
	}

	initiatorRaw := idMatches[len(idMatches)-1]
	if thirteenDigitsRe.FindStringIndex(text)[0] == 0 {
		initiatorRaw = idMatches[0]
	}

	initiator := resolvePersonName(tx, initiatorRaw, "")

	var targets []string

	for _, raw := range idMatches {
		name := resolvePersonName(tx, raw, "")
		if name != "" && name != initiator && !slices.Contains(targets, name) {
			targets = append(targets, name)
		}
	}

	if initiator != "" && len(targets) > 0 {
		return fmt.Sprintf("%s added %s", initiator, strings.Join(targets, ", ")), true
	}

	if initiator != "" && len(idMatches) == 2 && normalizeAuthorID(idMatches[0]) == normalizeAuthorID(idMatches[1]) {
		return initiator + " left", true
	}

	if initiator != "" {
		return initiator + " updated conversation", true
	}

	return "", false
}

func parseSeconds(raw string) int {
	val, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}

	return val
}

func formatDurationSeconds(sec int) string {
	if sec <= 0 {
		return ""
	}

	h := sec / 3600
	m := (sec % 3600) / 60
	s := sec % 60

	var parts []string

	if h > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}

	if m > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}

	if s > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", s))
	}

	return strings.Join(parts, " ")
}

func firstSubmatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) > 1 {
		return strings.TrimSpace(m[1])
	}

	return ""
}

func FormatReactionsSummary(reactions []ReactionPayload) *string {
	if len(reactions) == 0 {
		return nil
	}

	parts := make([]string, 0, len(reactions))
	for _, r := range reactions {
		label := r.Key
		if r.Emoji != nil && *r.Emoji != "" {
			label = *r.Emoji
		}

		parts = append(parts, fmt.Sprintf("%s %d", label, r.Count))
	}

	summary := strings.Join(parts, ", ")

	return &summary
}

func ExtractChatSvcReactions(raw any) []ReactionPayload {
	data := marshalOrBytes(raw)
	if len(data) == 0 {
		return nil
	}

	var entries []struct {
		Key   string `json:"key"`
		Users []any  `json:"users"`
		Count int64  `json:"count"`
	}

	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}

	var out []ReactionPayload

	for _, e := range entries {
		count := int64(len(e.Users))
		if count == 0 {
			count = e.Count
		}

		if e.Key != "" && count > 0 {
			em := EmojiFromItemID(e.Key)
			out = append(out, ReactionPayload{Key: e.Key, Emoji: &em, Count: count})
		}
	}

	return out
}

func ExtractChatSvcMentions(raw any) []MentionPayload {
	data := marshalOrBytes(raw)
	if len(data) == 0 {
		return nil
	}

	var entries []struct {
		MRI         string `json:"mri"`
		ID          string `json:"id"`
		UserID      string `json:"userId"`
		DisplayName string `json:"displayName"`
		Name        string `json:"name"`
	}

	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}

	var tokens []MentionPayload

	for _, e := range entries {
		id := normalizeAuthorID(firstNonEmptyString(e.MRI, e.ID, e.UserID))
		if id == "" {
			continue
		}

		name := firstNonEmptyString(e.DisplayName, e.Name)
		tokens = append(tokens, MentionPayload{ID: id, Name: nullIfEmpty(name)})
	}

	return MergeMentionTokens(tokens)
}

func MergeMentionTokens(tokens []MentionPayload) []MentionPayload {
	var order []string

	byID := make(map[string]MentionPayload)

	var (
		prevID    string
		prevParts []string
	)

	flush := func() {
		if prevID == "" {
			return
		}

		cand := strings.TrimSpace(strings.Join(prevParts, " "))
		existing, ok := byID[prevID]

		if !ok {
			order = append(order, prevID)
			byID[prevID] = MentionPayload{ID: prevID, Name: nullIfEmpty(cand)}
		} else if len(cand) > len(derefString(existing.Name)) {
			byID[prevID] = MentionPayload{ID: prevID, Name: nullIfEmpty(cand)}
		}

		prevID = ""
		prevParts = nil
	}

	for _, tok := range tokens {
		if tok.ID == "" {
			continue
		}

		if tok.ID != prevID {
			flush()

			prevID = tok.ID
		}

		name := strings.TrimSpace(derefString(tok.Name))
		if name != "" && !slices.Contains(prevParts, name) {
			prevParts = append(prevParts, name)
		}
	}

	flush()

	out := make([]MentionPayload, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}

	return out
}

func marshalOrBytes(raw any) []byte {
	if raw == nil {
		return nil
	}

	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}

		return []byte(v)
	case json.RawMessage:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil
		}

		return b
	}
}
