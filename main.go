package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"
)

const defaultTemplate = `{{.Greeting}}

{{.PersonalizedText}}

I am reaching out to explore whether there may be an opportunity to collaborate on a focused piece of work. The scope could be a single deliverable, a short project, or additional support for an existing initiative.

If this is relevant to your current priorities, I would be glad to learn more about your needs and see whether there is a practical fit.

Best regards,

{{.SenderName}}
{{.SenderTitle}}
{{.SenderEmail}}
{{.SenderWebsite}}
`

const defaultItalianTemplate = `{{.Greeting}}

{{.PersonalizedText}}

Vi contatto per capire se possa esserci l'opportunità di collaborare su un'attività ben definita. Potrebbe trattarsi di un singolo risultato, di un progetto breve oppure di un supporto aggiuntivo per un'iniziativa già avviata.

Se il tema è pertinente alle vostre priorità attuali, sarei lieto di conoscere meglio le vostre esigenze e verificare se esiste una collaborazione concreta possibile.

Cordiali saluti,

{{.SenderName}}
{{.SenderTitle}}
{{.SenderEmail}}
{{.SenderWebsite}}
`

type contact struct {
	Row              []string
	Email            string
	Company          string
	Greeting         string
	Subject          string
	PersonalizedText string
	Status           string
	Batch            string
	Language         string
	Audience         string
}

type templateData struct {
	Greeting         string
	Company          string
	Email            string
	Subject          string
	PersonalizedText string
	Batch            string
	Language         string
	Audience         string
	SenderName       string
	SenderTitle      string
	SenderEmail      string
	SenderWebsite    string
}

type smtpConfig struct {
	Host        string
	Port        int
	User        string
	Pass        string
	From        string
	FromName    string
	ReplyTo     string
	ImplicitTLS bool
}

type sentSets struct {
	Emails    map[string]bool
	Companies map[string]bool
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
var commonSuffixes = regexp.MustCompile(`\b(gmbh|srl|s\.r\.l|spa|s\.p\.a|ag|ltd|limited|bv|oy|ab|sas|inc|llc)\b`)

type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	*l = append(*l, splitList(v)...)
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	envPath := localEnvPath()
	if err := loadEnvFile(envPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fatalf("env file %s: %v", envPath, err)
	}

	var attachments listFlag
	var (
		contactsPath = flag.String("contacts", os.Getenv("MAIL_CONTACTS"), "CSV file containing contacts (default: MAIL_CONTACTS from .env)")
		templatePath = flag.String("template", os.Getenv("MAIL_TEMPLATE"), "Optional body template shared by both languages (default: MAIL_TEMPLATE)")
		outputPath   = flag.String("output", "contacts_updated.csv", "Output CSV with updated Status/SentAt values")
		logPath      = flag.String("log", "sent_log.csv", "Persistent send log used for duplicate protection")
		send         = flag.Bool("send", false, "Actually send email. Without this flag the program is dry-run only")
		limit        = flag.Int("limit", 10, "Maximum eligible emails per run; 0 means unlimited")
		delay        = flag.Duration("delay", 3*time.Second, "Delay between successful sends")
		eligible     = flag.String("eligible-status", "Not contacted", "Only rows with this Status are eligible")
		allowGeneric = flag.Bool("allow-generic", false, "Allow generic company mailboxes such as info@ / contact@")
		previewDir   = flag.String("preview-dir", "", "Optional directory where dry-run .eml previews are written")
		toAddr       = flag.String("to", "", "Single-email mode: recipient address (no contacts CSV needed)")
		toCompany    = flag.String("company", "", "Single-email mode: company name")
		toSubject    = flag.String("subject", "", "Single-email mode: subject")
		toGreeting   = flag.String("greeting", "", "Single-email mode: greeting line (default: Hello <company> team,)")
		toText       = flag.String("text", "", "Single-email mode: personalized paragraph")
		check        = flag.Bool("check", false, "Run preflight sanity checks only (no sending, no files written) and exit")
		yes          = flag.Bool("yes", false, "Skip the interactive confirmation before sending")
		interactive  = flag.Bool("interactive", false, "Guided setup for a CSV campaign or a single email")
		templateIT   = flag.String("template-it", os.Getenv("MAIL_TEMPLATE_IT"), "Italian body template (overrides --template for Italian contacts)")
		templateEN   = flag.String("template-en", os.Getenv("MAIL_TEMPLATE_EN"), "English body template (overrides --template for English contacts)")
		language     = flag.String("language", firstNonEmpty(os.Getenv("MAIL_LANGUAGE"), "en"), "Default language when the CSV has no language; also used with --to (it/en)")
		languageCol  = flag.String("language-column", "", "CSV language column (default: auto-detect Language, Lingua or Lang)")
		audience     = flag.String("audience", firstNonEmpty(os.Getenv("MAIL_AUDIENCE"), "nontechnical"), "Default audience: technical or nontechnical (CSV Audience takes precedence)")
	)
	flag.Var(&attachments, "attachment", "File to attach to every email; repeatable or comma-separated (default: MAIL_ATTACHMENTS from .env)")
	flag.Parse()

	in := bufio.NewReader(os.Stdin)
	if *interactive || (len(os.Args) == 1 && stdinIsTerminal()) {
		opts := wizardOptions{
			contacts: *contactsPath, to: *toAddr, company: *toCompany, greeting: *toGreeting,
			subject: *toSubject, text: *toText, language: *language, languageColumn: *languageCol, audience: *audience,
			template: *templatePath, templateIT: *templateIT, templateEN: *templateEN,
			attachments: attachments, eligible: *eligible, limit: *limit, delay: *delay,
			output: *outputPath, previewDir: *previewDir, log: *logPath, send: *send, check: *check,
		}
		if err := configureWizard(&console{in: in, out: os.Stdout}, &opts); err != nil {
			fatalf("procedura guidata interrotta: %v", err)
		}
		*contactsPath, *toAddr, *toCompany, *toGreeting = opts.contacts, opts.to, opts.company, opts.greeting
		*toSubject, *toText, *language, *languageCol = opts.subject, opts.text, opts.language, opts.languageColumn
		*audience = opts.audience
		*templatePath, *templateIT, *templateEN = opts.template, opts.templateIT, opts.templateEN
		attachments, *eligible, *limit, *delay = opts.attachments, opts.eligible, opts.limit, opts.delay
		*outputPath, *previewDir, *logPath, *send, *check = opts.output, opts.previewDir, opts.log, opts.send, opts.check
	}
	if flag.NArg() != 0 {
		fatalf("unexpected arguments: %s", strings.Join(flag.Args(), " "))
	}
	if *limit < 0 || *delay < 0 {
		fatalf("--limit and --delay must not be negative")
	}
	if *check {
		*send = false
	}
	if *toAddr == "" && *contactsPath == "" {
		fatalf("--contacts is required (or set MAIL_CONTACTS in .env), or use --to for a single email")
	}
	if len(attachments) == 0 {
		attachments = splitList(os.Getenv("MAIL_ATTACHMENTS"))
	}
	if len(attachments) == 0 {
		fatalf("--attachment is required (or set MAIL_ATTACHMENTS in .env)")
	}
	for _, a := range attachments {
		if info, err := os.Stat(a); err != nil {
			fatalf("attachment: %v", err)
		} else if !info.Mode().IsRegular() {
			fatalf("attachment is not a regular file: %s", a)
		}
	}
	attachNames := make([]string, len(attachments))
	for i, a := range attachments {
		attachNames[i] = filepath.Base(a)
	}

	templates, err := loadTemplates(*templatePath, *templateIT, *templateEN, *language)
	if err != nil {
		fatalf("templates: %v", err)
	}
	templates.defaultAudience, err = normalizeAudience(*audience)
	if err != nil {
		fatalf("audience: %v", err)
	}
	if *toAddr != "" {
		c := contact{Email: *toAddr, Company: *toCompany, Greeting: *toGreeting, Subject: *toSubject, PersonalizedText: *toText, Language: *language, Audience: *audience}
		runSingle(c, attachments, templates, *send, *check, *previewDir, *logPath, *yes, in)
		return
	}

	headers, rows, err := readCSV(*contactsPath)
	if err != nil {
		fatalf("read contacts: %v", err)
	}
	idx, err := requiredColumns(headers)
	if err != nil {
		fatalf("contacts CSV: %v", err)
	}
	if err := selectLanguageColumn(headers, idx, *languageCol); err != nil {
		fatalf("contacts CSV: %v", err)
	}

	sentAtIdx := ensureColumn(&headers, &rows, "SentAt")
	resultIdx := ensureColumn(&headers, &rows, "SendResult")

	prior, err := loadSentLog(*logPath)
	if err != nil {
		fatalf("load sent log: %v", err)
	}

	cfg := smtpConfig{}
	if *send {
		cfg, err = loadSMTPConfig()
		if err != nil {
			fatalf("SMTP configuration: %v", err)
		}
	}

	if *check || *send {
		ok := preflight(rows, idx, templates, prior, cfg, attachments, *eligible, *limit, *allowGeneric, *send)
		if *check {
			if !ok {
				os.Exit(1)
			}
			return
		}
		if !ok {
			fatalf("preflight found blocking problems; fix them and retry")
		}
		if !*yes && !confirmSend(in, wouldSend(rows, idx, prior, *eligible, *limit, *allowGeneric)) {
			fatalf("aborted, nothing was sent")
		}
	}
	if !*send && *previewDir != "" {
		if err := os.MkdirAll(*previewDir, 0o755); err != nil {
			fatalf("preview directory: %v", err)
		}
	}

	currentRun := sentSets{Emails: map[string]bool{}, Companies: map[string]bool{}}
	processed := 0
	successful := 0
	skipped := 0
	failed := 0

	for i := range rows {
		c := contactFromRow(rows[i], idx)
		rowNumber := i + 2

		if !strings.EqualFold(strings.TrimSpace(c.Status), strings.TrimSpace(*eligible)) {
			continue
		}
		if *limit > 0 && processed >= *limit {
			break
		}

		reason := duplicateReason(c, prior, currentRun)
		if reason != "" {
			rows[i][resultIdx] = "Skipped: " + reason
			fmt.Printf("SKIP row %d | %s | %s | %s\n", rowNumber, c.Company, c.Email, reason)
			skipped++
			continue
		}

		if !*allowGeneric && isGenericMailbox(c.Email) {
			rows[i][resultIdx] = "Skipped: generic mailbox disabled"
			fmt.Printf("SKIP row %d | %s | %s | generic mailbox\n", rowNumber, c.Company, c.Email)
			skipped++
			continue
		}

		addr, err := mail.ParseAddress(strings.TrimSpace(c.Email))
		if err != nil || addr.Address == "" {
			rows[i][resultIdx] = "Failed: invalid email"
			fmt.Printf("ERROR row %d | %s | invalid email %q\n", rowNumber, c.Company, c.Email)
			failed++
			continue
		}
		c.Email = addr.Address

		c, body, err := templates.render(c)
		if err != nil {
			rows[i][resultIdx] = "Failed: " + compactError(err)
			fmt.Printf("ERROR row %d | %s | template: %v\n", rowNumber, c.Company, err)
			failed++
			continue
		}

		messageID := newMessageID(domainFromEmail(firstNonEmpty(cfg.From, "sender@example.com")))
		fromAddr := firstNonEmpty(cfg.From, "sender@example.com")
		fromName := firstNonEmpty(cfg.FromName, "Your Name")
		msg, err := buildMessage(fromName, fromAddr, cfg.ReplyTo, c.Email, c.Subject, body, attachments, messageID)
		if err != nil {
			rows[i][resultIdx] = "Failed: message build error"
			fmt.Printf("ERROR row %d | %s | build message: %v\n", rowNumber, c.Company, err)
			failed++
			continue
		}

		processed++

		if !*send {
			fmt.Printf("\n--- DRY RUN %d ---\nTo: %s\nCompany: %s\nLanguage: %s\nAudience: %s\nTemplate: %s\nSubject: %s\nAttachment: %s\n\n%s\n", processed, c.Email, c.Company, c.Language, c.Audience, templates.sources[c.Language], c.Subject, strings.Join(attachNames, ", "), body)
			if *previewDir != "" {
				p := filepath.Join(*previewDir, fmt.Sprintf("%03d_%s.eml", processed, safeFilename(c.Company)))
				if err := os.WriteFile(p, msg, 0o644); err != nil {
					fmt.Printf("WARN preview write failed: %v\n", err)
				}
			}
			rows[i][resultIdx] = "Dry run only"
			currentRun.Emails[normalizeEmail(c.Email)] = true
			currentRun.Companies[normalizeCompany(c.Company)] = true
			continue
		}

		if err := sendSMTP(cfg, c.Email, msg); err != nil {
			failed++
			rows[i][resultIdx] = "Failed: " + compactError(err)
			_ = appendLog(*logPath, c, time.Now(), messageID, "Failed", err.Error())
			fmt.Printf("ERROR row %d | %s | %s | %v\n", rowNumber, c.Company, c.Email, err)
			continue
		}

		now := time.Now()
		successful++
		rows[i][idx["status"]] = "Sent"
		rows[i][sentAtIdx] = now.Format(time.RFC3339)
		rows[i][resultIdx] = "Sent"
		currentRun.Emails[normalizeEmail(c.Email)] = true
		currentRun.Companies[normalizeCompany(c.Company)] = true
		prior.Emails[normalizeEmail(c.Email)] = true
		prior.Companies[normalizeCompany(c.Company)] = true

		if err := appendLog(*logPath, c, now, messageID, "Sent", ""); err != nil {
			fmt.Printf("WARN sent, but failed to append log: %v\n", err)
		}
		fmt.Printf("SENT %d/%d | %s | %s\n", successful, processed, c.Company, c.Email)

		if *delay > 0 {
			time.Sleep(*delay)
		}
	}

	if err := writeCSV(*outputPath, headers, rows); err != nil {
		fatalf("write output CSV: %v", err)
	}

	mode := "DRY RUN"
	if *send {
		mode = "SEND"
	}
	fmt.Printf("\n%s complete. Eligible processed: %d | Sent: %d | Skipped: %d | Failed: %d\n", mode, processed, successful, skipped, failed)
	fmt.Printf("Updated CSV: %s\n", *outputPath)
	if *send {
		fmt.Printf("Persistent send log: %s\n", *logPath)
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func runSingle(c contact, attachments []string, templates *templateSet, send, check bool, previewDir, logPath string, yes bool, in *bufio.Reader) {
	addr, err := mail.ParseAddress(strings.TrimSpace(c.Email))
	if err != nil || addr.Address == "" {
		fatalf("--to: invalid email %q", c.Email)
	}
	c.Email = addr.Address
	if strings.TrimSpace(c.Company) == "" {
		c.Company = domainFromEmail(c.Email)
	}
	c, body, err := templates.render(c)
	if err != nil {
		fatalf("template: %v", err)
	}
	if check {
		fmt.Printf("CHECK OK | %s | %s | language: %s | audience: %s | template: %s\n", c.Company, c.Email, c.Language, c.Audience, templates.sources[c.Language])
		return
	}
	cfg := smtpConfig{}
	if send {
		if cfg, err = loadSMTPConfig(); err != nil {
			fatalf("SMTP configuration: %v", err)
		}
	}
	fromAddr := firstNonEmpty(cfg.From, "sender@example.com")
	fromName := firstNonEmpty(cfg.FromName, "Your Name")
	messageID := newMessageID(domainFromEmail(fromAddr))
	msg, err := buildMessage(fromName, fromAddr, cfg.ReplyTo, c.Email, c.Subject, body, attachments, messageID)
	if err != nil {
		fatalf("build message: %v", err)
	}
	if !send {
		fmt.Printf("--- DRY RUN ---\nTo: %s\nCompany: %s\nLanguage: %s\nAudience: %s\nTemplate: %s\nSubject: %s\n\n%s\n", c.Email, c.Company, c.Language, c.Audience, templates.sources[c.Language], c.Subject, body)
		if previewDir != "" {
			if err := os.MkdirAll(previewDir, 0o755); err != nil {
				fatalf("preview directory: %v", err)
			}
			p := filepath.Join(previewDir, "single_"+safeFilename(c.Company)+".eml")
			if err := os.WriteFile(p, msg, 0o644); err != nil {
				fatalf("preview write: %v", err)
			}
		}
		return
	}
	if send {
		fmt.Printf("About to SEND 1 email\n  From: %s <%s> via %s:%d\n  To: %s (%s)\n  Subject: %s\n  Attachments: %s\n",
			fromName, fromAddr, cfg.Host, cfg.Port, c.Email, c.Company, c.Subject, strings.Join(attachments, ", "))
		if !yes && !confirmSend(in, 1) {
			fatalf("aborted, nothing was sent")
		}
	}
	if err := sendSMTP(cfg, c.Email, msg); err != nil {
		_ = appendLog(logPath, c, time.Now(), messageID, "Failed", err.Error())
		fatalf("send: %v", err)
	}
	if err := appendLog(logPath, c, time.Now(), messageID, "Sent", ""); err != nil {
		fmt.Printf("WARN sent, but failed to append log: %v\n", err)
	}
	fmt.Printf("SENT | %s | %s\n", c.Company, c.Email)
}

func readCSV(path string) ([]string, [][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReader(f))
	r.FieldsPerRecord = -1
	headers, err := r.Read()
	if err != nil {
		return nil, nil, err
	}
	headers = normalizeHeaders(headers)

	var rows [][]string
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if len(row) < len(headers) {
			row = append(row, make([]string, len(headers)-len(row))...)
		}
		rows = append(rows, row)
	}
	return headers, rows, nil
}

func normalizeHeaders(h []string) []string {
	out := make([]string, len(h))
	for i, s := range h {
		out[i] = strings.TrimPrefix(strings.TrimSpace(s), "\ufeff")
	}
	return out
}

func requiredColumns(headers []string) (map[string]int, error) {
	wanted := []string{"Email", "Company", "Greeting", "Subject", "PersonalizedText", "Status", "Batch"}
	idx := map[string]int{}
	lower := map[string]int{}
	for i, h := range headers {
		lower[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, w := range wanted {
		i, ok := lower[strings.ToLower(w)]
		if !ok {
			return nil, fmt.Errorf("missing required column %q", w)
		}
		idx[strings.ToLower(w)] = i
	}
	for _, name := range []string{"audience", "pubblico", "tipodestinatario"} {
		if i, ok := lower[name]; ok {
			idx["audience"] = i
			break
		}
	}
	return idx, nil
}

func selectLanguageColumn(headers []string, idx map[string]int, name string) error {
	delete(idx, "language")
	names := []string{"Language", "Lingua", "Lang"}
	if strings.TrimSpace(name) != "" {
		names = []string{strings.TrimSpace(name)}
	}
	for _, candidate := range names {
		for i, header := range headers {
			if strings.EqualFold(strings.TrimSpace(header), candidate) {
				idx["language"] = i
				return nil
			}
		}
	}
	if strings.TrimSpace(name) != "" {
		return fmt.Errorf("missing language column %q", name)
	}
	return nil
}

func ensureColumn(headers *[]string, rows *[][]string, name string) int {
	for i, h := range *headers {
		if strings.EqualFold(strings.TrimSpace(h), name) {
			for j := range *rows {
				if len((*rows)[j]) <= i {
					(*rows)[j] = append((*rows)[j], make([]string, i-len((*rows)[j])+1)...)
				}
			}
			return i
		}
	}
	*headers = append(*headers, name)
	for i := range *rows {
		(*rows)[i] = append((*rows)[i], "")
	}
	return len(*headers) - 1
}

func contactFromRow(row []string, idx map[string]int) contact {
	get := func(name string) string {
		i, ok := idx[strings.ToLower(name)]
		if !ok || i < 0 || i >= len(row) {
			return ""
		}
		return strings.TrimSpace(row[i])
	}
	return contact{
		Row:              row,
		Email:            get("Email"),
		Company:          get("Company"),
		Greeting:         get("Greeting"),
		Subject:          get("Subject"),
		PersonalizedText: get("PersonalizedText"),
		Status:           get("Status"),
		Batch:            get("Batch"),
		Language:         get("Language"),
		Audience:         get("Audience"),
	}
}

func normalizeLanguage(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "it", "ita", "it-it", "italian", "italiano", "italiana":
		return "it", nil
	case "en", "eng", "en-us", "en-gb", "english", "inglese":
		return "en", nil
	default:
		return "", fmt.Errorf("unsupported language %q: use it or en", value)
	}
}

func normalizeAudience(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "technical", "tech", "tecnico", "tecnici":
		return "technical", nil
	case "nontechnical", "non-technical", "non tecnico", "non tecnici", "non_tecnico", "non-tecnico", "business":
		return "nontechnical", nil
	default:
		return "", fmt.Errorf("unsupported audience %q: use technical or nontechnical", value)
	}
}

type templateSet struct {
	defaultLanguage string
	defaultAudience string
	byLanguage      map[string]*template.Template
	sources         map[string]string
}

func loadTemplates(common, italian, english, defaultLanguage string) (*templateSet, error) {
	lang, err := normalizeLanguage(defaultLanguage)
	if err != nil {
		return nil, err
	}
	set := &templateSet{defaultLanguage: lang, defaultAudience: "nontechnical", byLanguage: map[string]*template.Template{}, sources: map[string]string{}}
	for _, entry := range []struct{ language, path string }{{"it", italian}, {"en", english}} {
		path := firstNonEmpty(entry.path, common)
		tpl, err := loadTemplate(path, entry.language)
		if err != nil {
			return nil, fmt.Errorf("%s (%s): %w", entry.language, path, err)
		}
		set.byLanguage[entry.language] = tpl
		set.sources[entry.language] = firstNonEmpty(path, "built-in ("+entry.language+")")
	}
	return set, nil
}

func (set *templateSet) render(c contact) (contact, string, error) {
	lang, err := normalizeLanguage(firstNonEmpty(c.Language, set.defaultLanguage))
	if err != nil {
		return c, "", err
	}
	c.Language = lang
	c.Audience, err = normalizeAudience(firstNonEmpty(c.Audience, set.defaultAudience))
	if err != nil {
		return c, "", err
	}
	c = contactDefaults(c)
	body, err := renderBody(set.byLanguage[lang], c)
	return c, body, err
}

func contactDefaults(c contact) contact {
	if strings.TrimSpace(c.Greeting) == "" {
		if c.Language == "it" {
			c.Greeting = "Gentile team di " + firstNonEmpty(c.Company, "riferimento") + ","
		} else {
			c.Greeting = "Hello " + firstNonEmpty(c.Company, "there") + " team,"
		}
	}
	if strings.TrimSpace(c.Subject) == "" {
		if c.Language == "it" {
			c.Subject = "Possibile collaborazione"
		} else {
			c.Subject = "Potential collaboration"
		}
	}
	return c
}

func loadTemplate(path, language string) (*template.Template, error) {
	text := defaultTemplate
	if language == "it" {
		text = defaultItalianTemplate
	}
	if strings.TrimSpace(path) != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		text = string(b)
	}
	return template.New("email").Option("missingkey=zero").Parse(text)
}

func renderBody(tpl *template.Template, c contact) (string, error) {
	c = contactDefaults(c)
	var b bytes.Buffer
	err := tpl.Execute(&b, templateData{
		Greeting:         c.Greeting,
		Company:          c.Company,
		Email:            c.Email,
		Subject:          c.Subject,
		PersonalizedText: c.PersonalizedText,
		Batch:            c.Batch,
		Language:         c.Language,
		Audience:         c.Audience,
		SenderName:       os.Getenv("MAIL_FROM_NAME"),
		SenderTitle:      os.Getenv("MAIL_SENDER_TITLE"),
		SenderEmail:      os.Getenv("MAIL_FROM"),
		SenderWebsite:    os.Getenv("MAIL_SENDER_WEBSITE"),
	})
	return strings.TrimSpace(b.String()) + "\n", err
}

// loadEnvFile reads KEY=VALUE lines from path into the process environment.
// Variables already set in the environment are not overridden.
func localEnvPath() string {
	if path := strings.TrimSpace(os.Getenv("MAIL_ENV_FILE")); path != "" {
		return path
	}
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}
	if _, err := os.Stat(".env.local"); err == nil {
		return ".env.local"
	}
	return ".env"
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func loadSMTPConfig() (smtpConfig, error) {
	port, err := strconv.Atoi(firstNonEmpty(os.Getenv("MAIL_SMTP_PORT"), "587"))
	if err != nil {
		return smtpConfig{}, fmt.Errorf("MAIL_SMTP_PORT: %w", err)
	}
	cfg := smtpConfig{
		Host:        strings.TrimSpace(os.Getenv("MAIL_SMTP_HOST")),
		Port:        port,
		User:        strings.TrimSpace(os.Getenv("MAIL_SMTP_USER")),
		Pass:        os.Getenv("MAIL_SMTP_PASS"),
		From:        strings.TrimSpace(os.Getenv("MAIL_FROM")),
		FromName:    strings.TrimSpace(os.Getenv("MAIL_FROM_NAME")),
		ReplyTo:     strings.TrimSpace(os.Getenv("MAIL_REPLY_TO")),
		ImplicitTLS: strings.EqualFold(strings.TrimSpace(os.Getenv("MAIL_SMTP_TLS")), "implicit") || port == 465,
	}
	if cfg.Host == "" || cfg.From == "" {
		return smtpConfig{}, errors.New("MAIL_SMTP_HOST and MAIL_FROM are required in --send mode")
	}
	if _, err := mail.ParseAddress(cfg.From); err != nil {
		return smtpConfig{}, fmt.Errorf("MAIL_FROM: %w", err)
	}
	return cfg, nil
}

func buildMessage(fromName, fromAddr, replyTo, to, subject, body string, attachmentPaths []string, messageID string) ([]byte, error) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)

	safeSubject := sanitizeHeader(subject)
	safeFromName := sanitizeHeader(fromName)

	fromHeader := (&mail.Address{Name: safeFromName, Address: fromAddr}).String()
	fmt.Fprintf(&b, "From: %s\r\n", fromHeader)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", safeSubject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s>\r\n", messageID)
	if strings.TrimSpace(replyTo) != "" {
		fmt.Fprintf(&b, "Reply-To: %s\r\n", sanitizeHeader(replyTo))
	}
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=%q\r\n", mw.Boundary())
	fmt.Fprintf(&b, "\r\n")

	textHeader := make(textproto.MIMEHeader)
	textHeader.Set("Content-Type", `text/plain; charset="UTF-8"`)
	textHeader.Set("Content-Transfer-Encoding", "8bit")
	textPart, err := mw.CreatePart(textHeader)
	if err != nil {
		return nil, err
	}
	normalizedBody := strings.ReplaceAll(body, "\n", "\r\n")
	if _, err := io.WriteString(textPart, normalizedBody); err != nil {
		return nil, err
	}

	for _, attachmentPath := range attachmentPaths {
		if err := writeAttachment(mw, attachmentPath); err != nil {
			return nil, err
		}
	}

	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeAttachment(mw *multipart.Writer, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	name := sanitizeHeader(filepath.Base(path))
	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", ctype+`; name="`+name+`"`)
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	h.Set("Content-Transfer-Encoding", "base64")
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	enc := base64.NewEncoder(base64.StdEncoding, newBase64LineWriter(part))
	if _, err := io.Copy(enc, file); err != nil {
		return err
	}
	return enc.Close()
}

func sendSMTP(cfg smtpConfig, to string, msg []byte) error {
	address := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}

	var client *smtp.Client
	var err error
	if cfg.ImplicitTLS {
		conn, err := tls.Dial("tcp", address, tlsCfg)
		if err != nil {
			return err
		}
		client, err = smtp.NewClient(conn, cfg.Host)
		if err != nil {
			_ = conn.Close()
			return err
		}
	} else {
		client, err = smtp.Dial(address)
		if err != nil {
			return err
		}
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsCfg); err != nil {
				_ = client.Close()
				return err
			}
		}
	}
	defer client.Close()

	if cfg.User != "" {
		auth := smtp.PlainAuth("", cfg.User, cfg.Pass, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP auth: %w", err)
		}
	}
	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func loadSentLog(path string) (sentSets, error) {
	sets := sentSets{Emails: map[string]bool{}, Companies: map[string]bool{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return sets, nil
	}
	if err != nil {
		return sets, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	headers, err := r.Read()
	if err != nil {
		return sets, err
	}
	pos := map[string]int{}
	for i, h := range headers {
		pos[strings.ToLower(strings.TrimSpace(h))] = i
	}
	eIdx, eOK := pos["email"]
	cIdx, cOK := pos["company"]
	resultIdx, hasResult := pos["result"]

	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return sets, err
		}
		if hasResult && resultIdx < len(row) && !strings.EqualFold(strings.TrimSpace(row[resultIdx]), "Sent") {
			continue
		}
		if eOK && eIdx < len(row) {
			if e := normalizeEmail(row[eIdx]); e != "" {
				sets.Emails[e] = true
			}
		}
		if cOK && cIdx < len(row) {
			if c := normalizeCompany(row[cIdx]); c != "" {
				sets.Companies[c] = true
			}
		}
	}
	return sets, nil
}

func appendLog(path string, c contact, when time.Time, messageID, result, detail string) error {
	newFile := false
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		newFile = true
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()
	if newFile {
		if err := w.Write([]string{"SentAt", "Company", "Email", "Batch", "MessageID", "Result", "Detail"}); err != nil {
			return err
		}
	}
	return w.Write([]string{when.Format(time.RFC3339), c.Company, c.Email, c.Batch, messageID, result, detail})
}

// classify mirrors the main loop's eligibility rules without side effects.
func classify(c contact, prior, sim sentSets, allowGeneric bool) (skip string, blocking bool) {
	if r := duplicateReason(c, prior, sim); r != "" {
		return r, false
	}
	if !allowGeneric && isGenericMailbox(c.Email) {
		return "generic mailbox disabled", false
	}
	if a, err := mail.ParseAddress(strings.TrimSpace(c.Email)); err != nil || a.Address == "" {
		return "invalid email", true
	}
	return "", false
}

func wouldSend(rows [][]string, idx map[string]int, prior sentSets, eligible string, limit int, allowGeneric bool) int {
	sim := sentSets{Emails: map[string]bool{}, Companies: map[string]bool{}}
	n := 0
	for _, row := range rows {
		c := contactFromRow(row, idx)
		if !strings.EqualFold(strings.TrimSpace(c.Status), strings.TrimSpace(eligible)) {
			continue
		}
		if limit > 0 && n >= limit {
			break
		}
		if skip, bad := classify(c, prior, sim, allowGeneric); skip != "" {
			_ = bad
			continue
		}
		n++
		sim.Emails[normalizeEmail(c.Email)] = true
		sim.Companies[normalizeCompany(c.Company)] = true
	}
	return n
}

// preflight prints a full report of what a run would do and returns false on blocking problems.
func preflight(rows [][]string, idx map[string]int, templates *templateSet, prior sentSets, cfg smtpConfig, attachments []string, eligible string, limit int, allowGeneric, send bool) bool {
	ok := true
	statuses := map[string]int{}
	languages := map[string]int{}
	audiences := map[string]int{}
	fallbackLanguage := 0
	sim := sentSets{Emails: map[string]bool{}, Companies: map[string]bool{}}
	var send_, dup, generic, invalid, noText, noSubject, noGreeting, tplErr, notEligible, overLimit int
	var problems []string
	var sample []string

	for i, row := range rows {
		c := contactFromRow(row, idx)
		rn := i + 2
		statuses[firstNonEmpty(c.Status, "(empty)")]++
		if !strings.EqualFold(strings.TrimSpace(c.Status), strings.TrimSpace(eligible)) {
			notEligible++
			continue
		}
		if limit > 0 && send_ >= limit {
			overLimit++
			continue
		}
		skip, bad := classify(c, prior, sim, allowGeneric)
		switch {
		case bad:
			invalid++
			problems = append(problems, fmt.Sprintf("row %d | %s | invalid email %q", rn, c.Company, c.Email))
			continue
		case skip == "generic mailbox disabled":
			generic++
			continue
		case skip != "":
			dup++
			problems = append(problems, fmt.Sprintf("row %d | %s | %s | duplicate: %s", rn, c.Company, c.Email, skip))
			continue
		}
		if c.PersonalizedText == "" {
			noText++
			problems = append(problems, fmt.Sprintf("row %d | %s | empty PersonalizedText", rn, c.Company))
		}
		if c.Subject == "" {
			noSubject++
		}
		if c.Greeting == "" {
			noGreeting++
		}
		if c.Language == "" {
			fallbackLanguage++
		}
		prepared, _, err := templates.render(c)
		if err != nil {
			tplErr++
			ok = false
			problems = append(problems, fmt.Sprintf("row %d | %s | template error: %v", rn, c.Company, err))
			continue
		}
		languages[prepared.Language]++
		audiences[prepared.Audience]++
		send_++
		sim.Emails[normalizeEmail(c.Email)] = true
		sim.Companies[normalizeCompany(c.Company)] = true
		if len(sample) < 5 {
			sample = append(sample, fmt.Sprintf("row %d | %s | %s | %s | %s | %s", rn, c.Company, c.Email, prepared.Language, prepared.Audience, templates.sources[prepared.Language]))
		}
	}

	printBrand(os.Stdout)
	fmt.Println("  PREFLIGHT SUMMARY")
	fmt.Println("  ------------------------------------------------------------")
	fmt.Printf("Rows in list:            %d\n", len(rows))
	var keys []string
	for k := range statuses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  Status %-18q %d\n", k, statuses[k])
	}
	fmt.Printf("Not eligible (status):   %d\n", notEligible)
	fmt.Printf("Already sent/duplicate:  %d\n", dup)
	fmt.Printf("Generic mailbox skipped: %d\n", generic)
	fmt.Printf("Invalid email:           %d\n", invalid)
	fmt.Printf("Template errors:         %d\n", tplErr)
	fmt.Println()
	fmt.Println("Message routing")
	fmt.Printf("Languages:               it=%d | en=%d | default (%s) used=%d\n", languages["it"], languages["en"], templates.defaultLanguage, fallbackLanguage)
	fmt.Printf("Audiences:               technical=%d | nontechnical=%d | default=%s\n", audiences["technical"], audiences["nontechnical"], templates.defaultAudience)
	fmt.Printf("Italian template:        %s\nEnglish template:        %s\n", templates.sources["it"], templates.sources["en"])
	fmt.Printf("Eligible left over limit:%d (limit %d)\n", overLimit, limit)
	fmt.Printf("Missing PersonalizedText:%d | default Subject: %d | default Greeting: %d\n", noText, noSubject, noGreeting)
	fmt.Println()
	fmt.Printf(">>> WILL SEND THIS RUN:  %d\n", send_)
	if send {
		fmt.Printf("SMTP: %s:%d as %q | From: %s | implicit TLS: %v\n", cfg.Host, cfg.Port, cfg.User, cfg.From, cfg.ImplicitTLS)
	}

	var total int64
	for _, a := range attachments {
		if st, err := os.Stat(a); err == nil {
			total += st.Size()
			fmt.Printf("Attachment: %s (%.1f KB)\n", a, float64(st.Size())/1024)
		}
	}
	if total > 10<<20 {
		fmt.Printf("WARN attachments total %.1f MB; many providers reject large mails\n", float64(total)/(1<<20))
	}
	if send_ == 0 {
		fmt.Println("BLOCKING: nothing to send")
		ok = false
	}
	if len(sample) > 0 {
		fmt.Println()
		fmt.Println("Review sample")
		for _, s := range sample {
			fmt.Println("  " + s)
		}
	}
	if len(problems) > 0 {
		fmt.Println()
		fmt.Println("Issues requiring attention")
		for i, p := range problems {
			if i >= 20 {
				fmt.Printf("  ... and %d more\n", len(problems)-20)
				break
			}
			fmt.Println("  " + p)
		}
	}
	fmt.Println("  ------------------------------------------------------------")
	return ok
}

type wizardOptions struct {
	contacts, to, company, greeting, subject, text string
	language, languageColumn                       string
	audience                                       string
	template, templateIT, templateEN               string
	attachments                                    []string
	eligible, output, previewDir, log              string
	limit                                          int
	delay                                          time.Duration
	send, check                                    bool
}

type console struct {
	in  *bufio.Reader
	out io.Writer
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func printBrand(out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  ENVELOPELY")
	fmt.Fprintln(out, "  Review-first correspondence")
	fmt.Fprintln(out, "  ------------------------------------------------------------")
}

// ask retries invalid answers and aborts on EOF instead of silently accepting defaults.
func (ui *console) ask(label, def string, validate func(string) error) (string, error) {
	for {
		fmt.Fprintf(ui.out, "%s [%s]: ", label, def)
		line, err := ui.in.ReadString('\n')
		if err != nil && (err != io.EOF || line == "") {
			return "", err
		}
		value := firstNonEmpty(line, def)
		if validate != nil {
			if err := validate(value); err != nil {
				fmt.Fprintln(ui.out, "Valore non valido:", err)
				continue
			}
		}
		return value, nil
	}
}

func (ui *console) choice(label string, choices []string, def int) (int, error) {
	fmt.Fprintln(ui.out, "\n"+label)
	for i, choice := range choices {
		fmt.Fprintf(ui.out, "  %d) %s\n", i+1, choice)
	}
	value, err := ui.ask("Scelta", strconv.Itoa(def), func(value string) error {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > len(choices) {
			return fmt.Errorf("scegli un numero da 1 a %d", len(choices))
		}
		return nil
	})
	n, _ := strconv.Atoi(value)
	return n, err
}

func regularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s non è un file", path)
	}
	return nil
}

func (ui *console) chooseFile(label, def string, patterns []string, builtIn bool) (string, error) {
	var paths []string
	seen := map[string]bool{}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			if !seen[path] && regularFile(path) == nil {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	sort.Strings(paths)
	fmt.Fprintln(ui.out, "\n"+label)
	for i, path := range paths {
		fmt.Fprintf(ui.out, "  %d) %s\n", i+1, path)
	}
	hint := "Numero o percorso del file"
	if builtIn {
		hint += " (- = template integrato)"
		def = firstNonEmpty(def, "-")
	}
	resolve := func(value string) string {
		if n, err := strconv.Atoi(value); err == nil && n > 0 && n <= len(paths) {
			return paths[n-1]
		}
		return value
	}
	value, err := ui.ask(hint, def, func(value string) error {
		if builtIn && value == "-" {
			return nil
		}
		return regularFile(resolve(value))
	})
	if value == "-" && builtIn {
		return "", err
	}
	return resolve(value), err
}

func configureWizard(ui *console, opts *wizardOptions) error {
	printBrand(ui.out)
	fmt.Fprintln(ui.out, "  Guided setup | Enter keeps the suggested value | Ctrl+C cancels")
	modeDefault := 1
	if opts.to != "" {
		modeDefault = 2
	}
	mode, err := ui.choice("Cosa vuoi preparare?", []string{"Campagna da CSV", "Email singola"}, modeDefault)
	if err != nil {
		return err
	}
	if mode == 1 {
		opts.to = ""
		opts.contacts, err = ui.chooseFile("CSV dei destinatari", opts.contacts, []string{"contacts/*.csv", "*.csv"}, false)
		if err != nil {
			return err
		}
		headers, rows, err := readCSV(opts.contacts)
		if err != nil {
			return err
		}
		idx, err := requiredColumns(headers)
		if err != nil {
			return err
		}
		if err := selectLanguageColumn(headers, idx, opts.languageColumn); err != nil {
			return err
		}
		if i, ok := idx["language"]; ok {
			fmt.Fprintf(ui.out, "Lingua letta dalla colonna %q (it/en).\n", headers[i])
		} else {
			fmt.Fprintln(ui.out, "Colonna Language/Lingua assente: verrà usata la lingua predefinita.")
		}
		if i, ok := idx["audience"]; ok {
			fmt.Fprintf(ui.out, "Pubblico letto dalla colonna %q (technical/nontechnical).\n", headers[i])
		}
		counts := map[string]int{}
		for _, row := range rows {
			counts[contactFromRow(row, idx).Status]++
		}
		var statuses []string
		for status := range counts {
			if status != "" {
				statuses = append(statuses, status)
			}
		}
		sort.Strings(statuses)
		if len(statuses) == 0 {
			return errors.New("il CSV non contiene destinatari con uno stato valorizzato")
		}
		fmt.Fprintf(ui.out, "%d destinatari. Stati disponibili:\n", len(rows))
		for _, status := range statuses {
			fmt.Fprintf(ui.out, "  %s: %d\n", status, counts[status])
		}
		opts.eligible, err = ui.ask("Stato da elaborare", opts.eligible, func(value string) error {
			for _, status := range statuses {
				if strings.EqualFold(status, value) {
					return nil
				}
			}
			return fmt.Errorf("scegli uno degli stati elencati")
		})
		if err != nil {
			return err
		}
	} else {
		opts.to, err = ui.ask("Destinatario (email)", opts.to, func(value string) error {
			_, err := mail.ParseAddress(value)
			return err
		})
		if err != nil {
			return err
		}
		opts.company, err = ui.ask("Azienda", opts.company, nil)
		if err != nil {
			return err
		}
	}
	opts.language, err = ui.ask("Lingua predefinita (it/en; il valore nel CSV ha precedenza)", opts.language, func(value string) error {
		_, err := normalizeLanguage(value)
		return err
	})
	if err != nil {
		return err
	}
	opts.language, _ = normalizeLanguage(opts.language)
	audienceDefault := 1
	if normalized, _ := normalizeAudience(opts.audience); normalized == "technical" {
		audienceDefault = 2
	}
	audienceChoice, err := ui.choice("Pubblico predefinito (il valore Audience nel CSV ha precedenza)", []string{"Non tecnico: attività e benefici pratici", "Tecnico: competenze e tecnologie"}, audienceDefault)
	if err != nil {
		return err
	}
	opts.audience = "nontechnical"
	if audienceChoice == 2 {
		opts.audience = "technical"
	}
	if mode == 2 {
		defaults := contactDefaults(contact{Company: opts.company, Language: opts.language})
		opts.greeting, err = ui.ask("Saluto", firstNonEmpty(opts.greeting, defaults.Greeting), nil)
		if err != nil {
			return err
		}
		opts.subject, err = ui.ask("Oggetto", firstNonEmpty(opts.subject, defaults.Subject), nil)
		if err != nil {
			return err
		}
		fmt.Fprintln(ui.out, "Paragrafo personalizzato: termina con una riga vuota; Invio mantiene il testo attuale.")
		if opts.text != "" {
			fmt.Fprintln(ui.out, opts.text)
		}
		var lines []string
		for {
			line, err := ui.in.ReadString('\n')
			if err != nil {
				return err
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.TrimSpace(line) == "" {
				break
			}
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			opts.text = strings.Join(lines, "\n")
		}
	}
	templateDefault := 1
	if opts.template != "" && opts.templateIT == "" && opts.templateEN == "" {
		templateDefault = 2
	}
	templateMode, err := ui.choice("Template del messaggio", []string{"Due template: italiano e inglese", "Un template comune"}, templateDefault)
	if err != nil {
		return err
	}
	if templateMode == 1 {
		opts.template = ""
		if opts.templateIT == "" && regularFile("templates/email_template_italia.txt") == nil {
			opts.templateIT = "templates/email_template_italia.txt"
		}
		if opts.templateEN == "" && regularFile("templates/email_template_international.txt") == nil {
			opts.templateEN = "templates/email_template_international.txt"
		}
		opts.templateIT, err = ui.chooseFile("Template italiano", opts.templateIT, []string{"templates/*.txt", "*.txt"}, true)
		if err != nil {
			return err
		}
		opts.templateEN, err = ui.chooseFile("Template inglese", opts.templateEN, []string{"templates/*.txt", "*.txt"}, true)
	} else {
		opts.templateIT, opts.templateEN = "", ""
		opts.template, err = ui.chooseFile("Template comune", opts.template, []string{"templates/*.txt", "*.txt"}, true)
	}
	if err != nil {
		return err
	}
	if len(opts.attachments) == 0 {
		opts.attachments = splitList(os.Getenv("MAIL_ATTACHMENTS"))
	}
	attachmentText, err := ui.ask("Allegati (percorsi separati da virgole)", strings.Join(opts.attachments, ","), func(value string) error {
		paths := splitList(value)
		if len(paths) == 0 {
			return errors.New("indica almeno un allegato")
		}
		for _, path := range paths {
			if err := regularFile(path); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	opts.attachments = splitList(attachmentText)
	if mode == 1 {
		value, err := ui.ask("Numero massimo di email (0 = tutte)", strconv.Itoa(opts.limit), func(value string) error {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return errors.New("inserisci un intero maggiore o uguale a 0")
			}
			return nil
		})
		if err != nil {
			return err
		}
		opts.limit, _ = strconv.Atoi(value)
		value, err = ui.ask("Pausa tra gli invii (es. 3s, 1m)", opts.delay.String(), func(value string) error {
			d, err := time.ParseDuration(value)
			if err != nil || d < 0 {
				return errors.New("inserisci una durata non negativa, es. 3s")
			}
			return nil
		})
		if err != nil {
			return err
		}
		opts.delay, _ = time.ParseDuration(value)
		opts.output, err = ui.ask("CSV di risultato", opts.output, nil)
		if err != nil {
			return err
		}
	}
	opts.previewDir, err = ui.ask("Cartella anteprime .eml (- = nessuna)", firstNonEmpty(opts.previewDir, "previews"), nil)
	if err != nil {
		return err
	}
	if opts.previewDir == "-" {
		opts.previewDir = ""
	}
	opts.log, err = ui.ask("Registro invii", opts.log, nil)
	if err != nil {
		return err
	}
	actionDefault := 1
	if opts.send {
		actionDefault = 3
	}
	if opts.check {
		actionDefault = 2
	}
	actions := []string{"Anteprima (dry run)", "Solo controlli (nessun file scritto)", "Invio reale con conferma finale"}
	action, err := ui.choice("Operazione", actions, actionDefault)
	if err != nil {
		return err
	}
	opts.send, opts.check = action == 3, action == 2
	fmt.Fprintf(ui.out, "\nRiepilogo: %s\nDestinatari: %s\nLingua predefinita: %s\nPubblico predefinito: %s\nAllegati: %s\n", actions[action-1], firstNonEmpty(opts.to, opts.contacts), opts.language, opts.audience, strings.Join(opts.attachments, ", "))
	return nil
}

// confirmSend asks the user to type the exact number of emails about to be sent.
func confirmSend(in *bufio.Reader, n int) bool {
	fmt.Printf("Digita %d per confermare l'invio di %d email, qualsiasi altro valore annulla: ", n, n)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line) == strconv.Itoa(n) && n > 0
}

func duplicateReason(c contact, prior sentSets, current sentSets) string {
	e := normalizeEmail(c.Email)
	company := normalizeCompany(c.Company)
	if e != "" && prior.Emails[e] {
		return "email already present in sent log"
	}
	if company != "" && prior.Companies[company] {
		return "company already present in sent log"
	}
	if e != "" && current.Emails[e] {
		return "duplicate email within current run"
	}
	if company != "" && current.Companies[company] {
		return "duplicate company within current run"
	}
	return ""
}

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func normalizeCompany(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "&", " and ")
	s = commonSuffixes.ReplaceAllString(s, " ")
	s = nonAlnum.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

func isGenericMailbox(email string) bool {
	local := strings.ToLower(strings.SplitN(email, "@", 2)[0])
	generic := map[string]bool{
		"info": true, "contact": true, "hello": true, "sales": true, "support": true,
		"office": true, "team": true, "admin": true, "commerciale": true, "commerciali": true,
	}
	return generic[local]
}

func writeCSV(path string, headers []string, rows [][]string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if err := w.Write(headers); err != nil {
		return err
	}
	for _, row := range rows {
		if len(row) < len(headers) {
			row = append(row, make([]string, len(headers)-len(row))...)
		}
		if err := w.Write(row[:len(headers)]); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func newMessageID(domain string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d.%s", time.Now().UnixNano(), domain)
	}
	return fmt.Sprintf("%d.%s@%s", time.Now().UnixNano(), hex.EncodeToString(b), domain)
}

func domainFromEmail(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		return strings.TrimSpace(parts[1])
	}
	return "localhost"
}

func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func safeFilename(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "contact"
	}
	s = regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func compactError(err error) string {
	s := strings.ReplaceAll(err.Error(), "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 180 {
		return s[:180]
	}
	return s
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}

// base64LineWriter inserts CRLF every 76 base64 characters, which is friendly to mail transports.
type base64LineWriter struct {
	w     io.Writer
	count int
}

func newBase64LineWriter(w io.Writer) io.Writer {
	return &base64LineWriter{w: w}
}

func (lw *base64LineWriter) Write(p []byte) (int, error) {
	writtenInput := 0
	for len(p) > 0 {
		remaining := 76 - lw.count
		n := remaining
		if n > len(p) {
			n = len(p)
		}
		if _, err := lw.w.Write(p[:n]); err != nil {
			return writtenInput, err
		}
		lw.count += n
		writtenInput += n
		p = p[n:]
		if lw.count == 76 {
			if _, err := io.WriteString(lw.w, "\r\n"); err != nil {
				return writtenInput, err
			}
			lw.count = 0
		}
	}
	return writtenInput, nil
}
