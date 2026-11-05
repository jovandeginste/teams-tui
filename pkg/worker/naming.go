package worker

import (
	"regexp"
	"strings"

	"gorm.io/gorm"
)

var (
	uuidRe         = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	plusDigitsRe   = regexp.MustCompile(`\+\d+$`)
	tsOrgIDStartRe = regexp.MustCompile(`(?i)^\d{13}8:orgid:`)
	orgIDGlobalRe  = regexp.MustCompile(`(?i)8:orgid:[0-9a-f-]{36}`)
	leftSuffixRe   = regexp.MustCompile(`\s*(?:left|heeft de chat verlaten).*$`)
	parenSuffixRe  = regexp.MustCompile(`\s*\([^)]+\)\s*$`)
)

type channelInfoRow struct {
	Name     string `gorm:"column:name"`
	TeamName string `gorm:"column:team_name"`
	Type     string `gorm:"column:type"`
}

type namingMessageRow struct {
	ID         int64  `gorm:"column:id"`
	Direction  string `gorm:"column:direction"`
	AuthorID   string `gorm:"column:author_id"`
	AuthorName string `gorm:"column:author_name"`
	Content    string `gorm:"column:content"`
}

func upsertChannel(tx *gorm.DB, channelID string, rawName, channelType, rawTeamName *string) (string, error) {
	name := normalizeChannelNamePtr(rawName)
	teamName := normalizeChannelNamePtr(rawTeamName)
	chType := derefString(channelType)

	if channelID != "" {
		var existing channelInfoRow

		err := tx.Raw("SELECT COALESCE(name, '') AS name, COALESCE(team_name, '') AS team_name, COALESCE(type, '') AS type FROM channels WHERE id = ?", channelID).Scan(&existing).Error
		if err == nil {
			existingName := normalizeChannelName(existing.Name)
			if (chType == "group chat" || strings.HasSuffix(channelID, "@thread.v2")) && shouldKeepExistingGroupChannelName(existingName, name) {
				name = existingName
			} else if existingName != "" && teamName != "" && name == teamName && existingName != teamName {
				name = existingName
			}
		}
	}

	err := tx.Exec(`
		INSERT INTO channels (id, name, team_name, type) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = COALESCE(excluded.name, channels.name),
			team_name = COALESCE(excluded.team_name, channels.team_name),
			type = COALESCE(excluded.type, channels.type)
	`, channelID, nullIfEmpty(name), nullIfEmpty(teamName), nullIfEmpty(chType)).Error

	return name, err
}

func normalizeChannelNamePtr(raw *string) string {
	if raw == nil {
		return ""
	}

	return normalizeChannelName(*raw)
}

func normalizeChannelName(raw string) string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")

	var parts []string

	for line := range strings.SplitSeq(raw, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}

	return strings.Join(parts, ", ")
}

func shouldKeepExistingGroupChannelName(existingName, nextName string) bool {
	if existingName == "" || nextName == "" || existingName == nextName {
		return false
	}

	if plusDigitsRe.MatchString(nextName) && !plusDigitsRe.MatchString(existingName) {
		return true
	}

	existingParts := splitTrimmedCSV(existingName)
	nextParts := splitTrimmedCSV(nextName)

	existingHasFull := false
	for _, p := range existingParts {
		if strings.Contains(p, " ") {
			existingHasFull = true
			break
		}
	}

	if !existingHasFull || len(nextParts) == 0 {
		return false
	}

	existingFirsts := make(map[string]bool, len(existingParts))
	for _, p := range existingParts {
		fields := strings.Fields(p)
		if len(fields) > 0 {
			existingFirsts[strings.ToLower(fields[0])] = true
		}
	}

	for _, p := range nextParts {
		if strings.Contains(p, " ") || !existingFirsts[strings.ToLower(p)] {
			return false
		}
	}

	return true
}

func splitTrimmedCSV(s string) []string {
	var out []string

	for part := range strings.SplitSeq(s, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}

	return out
}

func isCanonicalChannelID(id string) bool {
	return strings.HasPrefix(id, "19:") || id == "48:notes"
}

func normalizeAuthorID(raw string) string {
	id := strings.TrimSpace(raw)
	if id == "" {
		return ""
	}

	if _, after, ok := strings.Cut(id, "/contacts/"); ok {
		id = after
	}

	if uuidRe.MatchString(id) {
		return "8:orgid:" + strings.ToLower(id)
	}

	return id
}

func resolvePersonName(tx *gorm.DB, rawID, fallback string) string {
	id := normalizeAuthorID(rawID)
	if id == "" {
		return fallback
	}

	if strings.HasPrefix(id, "28:app:") {
		if fallback != "" {
			return fallback
		}

		return "app"
	}

	if strings.HasPrefix(id, "28:") {
		if fallback != "" {
			return fallback
		}

		return "bot"
	}

	var name string

	if err := tx.Raw("SELECT COALESCE(name, '') FROM authors WHERE id = ?", id).Scan(&name).Error; err == nil && name != "" {
		return name
	}

	if fallback != "" {
		return fallback
	}

	return id
}

type participantTracker struct {
	tx      *gorm.DB
	myID    string
	myName  string
	order   []string
	active  map[string]bool
	all     map[string]bool
	removed map[string]bool
}

func newParticipantTracker(tx *gorm.DB, me *UserInfo) *participantTracker {
	t := &participantTracker{ //nolint:exhaustruct_v5
		tx:      tx,
		active:  make(map[string]bool),
		all:     make(map[string]bool),
		removed: make(map[string]bool),
	}

	if me != nil {
		t.myID = strings.TrimSpace(me.ID)
		t.myName = strings.TrimSpace(me.Name)
	}

	return t
}

func (t *participantTracker) add(raw string, force bool) {
	name := strings.TrimSpace(raw)
	if strings.HasPrefix(name, "8:") {
		name = resolvePersonName(t.tx, name, "")
	}

	if isIgnoredParticipant(name, t.myName) {
		return
	}

	if force {
		delete(t.removed, name)
	}

	if !t.all[name] {
		t.all[name] = true
		t.order = append(t.order, name)
	}

	if !t.removed[name] {
		t.active[name] = true
	}
}

func (t *participantTracker) remove(raw string) {
	name := strings.TrimSpace(raw)
	if strings.HasPrefix(name, "8:") {
		name = resolvePersonName(t.tx, name, "")
	}

	if name == "" {
		return
	}

	delete(t.active, name)

	t.removed[name] = true

	lower := strings.ToLower(name)
	for a := range t.active {
		fields := strings.Fields(a)
		if len(fields) > 0 && strings.ToLower(fields[0]) == lower {
			delete(t.active, a)

			t.removed[a] = true
		}
	}
}

func isIgnoredParticipant(name, myName string) bool {
	if name == "" || name == myName {
		return true
	}

	switch name {
	case "bot", "app", "Polls", "Everyone", "Conference Virtual Assistant":
		return true
	}

	return strings.HasPrefix(name, "8:") ||
		strings.HasPrefix(name, "28:") ||
		strings.HasPrefix(name, "4:") ||
		strings.HasPrefix(name, "R.ICTS.")
}

func computeGroupChatNameFromDB(tx *gorm.DB, channelID string, me *UserInfo) string {
	if !isCanonicalChannelID(channelID) {
		return ""
	}

	var rows []namingMessageRow

	err := tx.Raw(`
		SELECT m.id, COALESCE(m.direction, '') AS direction, COALESCE(m.author_id, '') AS author_id, COALESCE(a.name, '') AS author_name, COALESCE(m.content, '') AS content
		FROM messages m
		LEFT JOIN authors a ON a.id = m.author_id
		WHERE m.channel_id = ?
		ORDER BY m.id ASC
	`, channelID).Scan(&rows).Error
	if err != nil || len(rows) == 0 {
		return ""
	}

	tracker := newParticipantTracker(tx, me)

	for _, r := range rows {
		tracker.processRow(r)
	}

	deduped := dedupeParticipantNames(tracker.chosenNames(), tracker.myName)

	return strings.Join(deduped, ", ")
}

func (t *participantTracker) processRow(r namingMessageRow) {
	if r.Direction != "event" {
		if r.AuthorID != "" && (t.myID == "" || r.AuthorID != t.myID) {
			author := r.AuthorName
			if author == "" {
				author = resolvePersonName(t.tx, r.AuthorID, "")
			}

			t.add(author, false)
		}

		return
	}

	applyEventRowToTracker(t, r)
}

func (t *participantTracker) chosenNames() []string {
	var chosen []string

	for _, n := range t.order {
		if len(t.active) > 0 {
			if t.active[n] {
				chosen = append(chosen, n)
			}
		} else if t.all[n] {
			chosen = append(chosen, n)
		}
	}

	return chosen
}

func applyEventRowToTracker(t *participantTracker, r namingMessageRow) {
	text := strings.TrimSpace(r.Content)
	evMentions := selectEventMentions(t.tx, r.ID, t.myName)

	switch {
	case strings.Contains(text, " removed ") || strings.Contains(text, " verwijderd"):
		applyRemovedEvent(t, text, evMentions)
	case strings.HasSuffix(text, " left") || strings.Contains(text, " heeft de chat verlaten"):
		if len(evMentions) > 0 {
			for _, p := range evMentions {
				t.remove(p)
			}
		} else {
			t.remove(leftSuffixRe.ReplaceAllString(text, ""))
		}
	case strings.Contains(text, " added ") || strings.Contains(text, " toegevoegd"):
		applyAddedEvent(t, text, evMentions)
	case strings.HasPrefix(text, "Added ") || strings.HasPrefix(text, "Members added: "):
		rest := strings.TrimPrefix(strings.TrimPrefix(text, "Added "), "Members added: ")
		for _, p := range splitTrimmedCSV(rest) {
			t.add(p, true)
		}
	case tsOrgIDStartRe.MatchString(text):
		applyOrgIDEvent(t, text)
	case strings.HasPrefix(text, "Call ended — "):
		for _, p := range splitTrimmedCSV(strings.TrimPrefix(text, "Call ended — ")) {
			t.add(strings.TrimSpace(parenSuffixRe.ReplaceAllString(p, "")), false)
		}
	}
}

func applyRemovedEvent(t *participantTracker, text string, evMentions []string) {
	if before, after, ok := strings.Cut(text, " removed "); ok {
		t.add(strings.TrimSpace(before), false)

		for _, p := range splitTrimmedCSV(after) {
			t.remove(p)
		}

		return
	}

	if len(evMentions) >= 2 {
		t.add(evMentions[0], false)

		for _, p := range evMentions[1:] {
			t.remove(p)
		}
	}
}

func applyAddedEvent(t *participantTracker, text string, evMentions []string) {
	for _, p := range evMentions {
		t.add(p, true)
	}

	if before, after, ok := strings.Cut(text, " added "); ok {
		t.add(strings.TrimSpace(before), true)

		for _, p := range splitTrimmedCSV(after) {
			t.add(p, true)
		}
	}
}

func applyOrgIDEvent(t *participantTracker, text string) {
	matches := orgIDGlobalRe.FindAllString(text, -1)
	if len(matches) == 2 && strings.EqualFold(matches[0], matches[1]) {
		t.remove(strings.ToLower(matches[0]))

		return
	}

	for _, id := range matches {
		t.add(strings.ToLower(id), true)
	}
}

func selectEventMentions(tx *gorm.DB, messageID int64, myName string) []string {
	var names []string

	_ = tx.Raw(`
		SELECT COALESCE(a.name, '')
		FROM message_mentions mm
		JOIN authors a ON a.id = mm.author_id
		WHERE mm.message_id = ?
	`, messageID).Scan(&names).Error

	var out []string

	for _, n := range names {
		if n != "" && n != myName {
			out = append(out, n)
		}
	}

	return out
}

func dedupeParticipantNames(rawNames []string, myName string) []string {
	var cleaned []string

	seenLower := make(map[string]bool, len(rawNames))
	myLower := strings.ToLower(strings.TrimSpace(myName))

	for _, raw := range rawNames {
		name := strings.TrimSpace(raw)
		if isIgnoredParticipant(name, "") {
			continue
		}

		lower := strings.ToLower(name)
		if myLower != "" && lower == myLower {
			continue
		}

		if seenLower[lower] {
			continue
		}

		seenLower[lower] = true

		cleaned = append(cleaned, name)
	}

	multiWordFirsts := make(map[string]bool)
	for _, n := range cleaned {
		if strings.Contains(n, " ") {
			fields := strings.Fields(n)
			if len(fields) > 0 {
				multiWordFirsts[strings.ToLower(fields[0])] = true
			}
		}
	}

	var out []string

	for _, n := range cleaned {
		if strings.Contains(n, " ") || !multiWordFirsts[strings.ToLower(n)] {
			out = append(out, n)
		}
	}

	return out
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}
