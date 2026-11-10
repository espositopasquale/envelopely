package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"flag"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTemplateRouting(t *testing.T) {
	dir := t.TempDir()
	common := writeFixture(t, dir, "common.txt", "COMMON {{.Language}} {{.Subject}}")
	italian := writeFixture(t, dir, "it.txt", "ITALIAN {{.Language}} {{.Greeting}} {{.PersonalizedText}}")
	english := writeFixture(t, dir, "en.txt", "ENGLISH {{.Language}} {{.Greeting}} {{.PersonalizedText}}")
	for _, tc := range []struct {
		name, common, it, en, fallback, language, want string
	}{
		{"italian overrides common", common, italian, english, "en", " IT ", "ITALIAN it Gentile team di Acme, custom"},
		{"english overrides default", common, italian, english, "it", "English", "ENGLISH en Hello Acme team, custom"},
		{"blank uses default", common, italian, english, "it", "", "ITALIAN it Gentile team di Acme, custom"},
		{"common fallback", common, italian, "", "en", "en", "COMMON en Potential collaboration"},
		{"italian built in", "", "", "", "it", "", "Cordiali saluti,"},
		{"legacy english built in", "", "", "", "en", "", "Best regards,"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := loadTemplates(tc.common, tc.it, tc.en, tc.fallback)
			if err != nil {
				t.Fatal(err)
			}
			c, body, err := set.render(contact{Company: "Acme", Language: tc.language, PersonalizedText: "custom"})
			if err != nil || !strings.Contains(body, tc.want) {
				t.Fatalf("render = %q, %v; want %q", body, err, tc.want)
			}
			if c.Subject == "" || c.Greeting == "" || c.Language == "" {
				t.Fatalf("missing defaults: %+v", c)
			}
			original := contact{Language: "it", Greeting: "Ciao Anna,", Subject: "Progetto", PersonalizedText: "Testo originale"}
			prepared, _, err := set.render(original)
			if err != nil || prepared.Greeting != original.Greeting || prepared.Subject != original.Subject || prepared.PersonalizedText != original.PersonalizedText {
				t.Fatalf("CSV text changed: %+v, %v", prepared, err)
			}
		})
	}
	set, err := loadTemplates("", "", "", "en")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := set.render(contact{Language: "fr"}); err == nil {
		t.Fatal("unknown language accepted")
	}
	if _, err := loadTemplates("", "", "", "fr"); err == nil {
		t.Fatal("unknown default language accepted")
	}
}

func TestOptionalLanguageColumn(t *testing.T) {
	for _, tc := range []struct{ header, custom string }{
		{"Language", ""}, {" lingua ", ""}, {"LANG", ""}, {"ContactLanguage", "contactlanguage"}, {"", ""},
	} {
		t.Run(tc.header, func(t *testing.T) {
			headers := []string{"Email", "Company", "Greeting", "Subject", "PersonalizedText", "Status", "Batch"}
			row := []string{"first@example.com", "Acme", "", "", "", "Not contacted", "test"}
			if tc.header != "" {
				headers = append(headers, tc.header)
				row = append(row, "it")
			}
			idx, err := requiredColumns(headers)
			if err != nil {
				t.Fatal(err)
			}
			if err := selectLanguageColumn(headers, idx, tc.custom); err != nil {
				t.Fatal(err)
			}
			want := "it"
			if tc.header == "" {
				want = ""
			}
			if got := contactFromRow(row, idx).Language; got != want {
				t.Fatalf("language = %q; want %q", got, want)
			}
			if err := selectLanguageColumn(headers, idx, "missing"); err == nil {
				t.Fatal("missing explicit language column accepted")
			}
		})
	}
}

// Run the real CLI in an isolated process so exit codes, stdin, and file writes
// are exercised without inheriting the user's .env or touching real contacts.
func TestCLIProcess(t *testing.T) {
	if os.Getenv("MAILOUTREACH_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"envelopely"}, os.Args[i+1:]...)
			break
		}
	}
	flag.CommandLine = flag.NewFlagSet("envelopely", flag.ExitOnError)
	main()
	os.Exit(0)
}

func runCLI(t *testing.T, dir, input string, args ...string) (string, error) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "MAIL_") && !strings.HasPrefix(entry, "MAILOUTREACH_TEST_PROCESS=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "MAILOUTREACH_TEST_PROCESS=1", "MAIL_ENV_FILE=missing.env")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

const mixedCSV = "Email,Company,Greeting,Subject,PersonalizedText,Status,Batch,Language\n" +
	"it@example.com,Acme Italia,,,Testo italiano,Not contacted,test,it\n" +
	"en@example.com,Acme UK,,,English text,Not contacted,test,en\n"

func TestCLIMixedLanguagePreviews(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "contacts.csv", mixedCSV)
	writeFixture(t, dir, "cv.txt", "attachment")
	writeFixture(t, dir, "it.txt", "{{.Greeting}}\nITALIAN {{.PersonalizedText}}")
	writeFixture(t, dir, "en.txt", "{{.Greeting}}\nENGLISH {{.PersonalizedText}}")
	out, err := runCLI(t, dir, "", "--contacts", "contacts.csv", "--attachment", "cv.txt", "--template-it", "it.txt", "--template-en", "en.txt", "--preview-dir", "previews")
	if err != nil || !strings.Contains(out, "Eligible processed: 2") {
		t.Fatalf("CLI: %v\n%s", err, out)
	}
	for _, tc := range []struct{ file, subject, body string }{
		{"001_Acme_Italia.eml", "Possibile collaborazione", "ITALIAN Testo italiano"},
		{"002_Acme_UK.eml", "Potential collaboration", "ENGLISH English text"},
	} {
		content, err := os.ReadFile(filepath.Join(dir, "previews", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		message, err := mail.ReadMessage(bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
		if err != nil || subject != tc.subject {
			t.Fatalf("MIME subject = %q, %v", subject, err)
		}
		_, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		part, err := multipart.NewReader(message.Body, params["boundary"]).NextPart()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(part)
		if err != nil || !strings.Contains(string(body), tc.body) {
			t.Fatalf("MIME body = %q, %v", body, err)
		}
	}
	file, err := os.Open(filepath.Join(dir, "contacts_updated.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rows, err := csv.NewReader(file).ReadAll()
	if err != nil || rows[1][5] != "Not contacted" || rows[1][7] != "it" || rows[1][9] != "Dry run only" {
		t.Fatalf("unexpected output CSV: %v, %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sent_log.csv")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote a sent log")
	}
}

func TestCLICheckNoWritesAndLanguageErrors(t *testing.T) {
	for _, tc := range []struct {
		name, contacts string
		extra          []string
		want           string
		wantError      bool
	}{
		{"mixed", mixedCSV, nil, "it=1 | en=1", false},
		{"limit across languages", mixedCSV, []string{"--limit", "1"}, "it=1 | en=0", false},
		{"duplicates across languages", mixedCSV + "duplicate@example.com,Acme Italia,,,English text,Not contacted,test,en\n", nil, "Already sent/duplicate:  1", false},
		{"invalid language", strings.Replace(mixedCSV, ",it\n", ",fr\n", 1), nil, "unsupported language", true},
		{"legacy CSV", strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(mixedCSV, ",Language", ""), ",it\n", "\n"), ",en\n", "\n"), nil, "it=0 | en=2", false},
		{"default Italian", strings.ReplaceAll(mixedCSV, ",it\n", ",\n"), []string{"--language", "it"}, "it=1 | en=1", false},
		{"custom column", strings.Replace(mixedCSV, "Language", "ContactLang", 1), []string{"--language-column", "contactlang"}, "it=1 | en=1", false},
		{"single check wins over send", mixedCSV, []string{"--to", "one@example.com", "--language", "it"}, "CHECK OK", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFixture(t, dir, "contacts.csv", tc.contacts)
			writeFixture(t, dir, "cv.txt", "attachment")
			args := append([]string{"--contacts", "contacts.csv", "--attachment", "cv.txt", "--check", "--send", "--preview-dir", "previews"}, tc.extra...)
			out, err := runCLI(t, dir, "", args...)
			if (err != nil) != tc.wantError || !strings.Contains(out, tc.want) {
				t.Fatalf("CLI: %v\n%s", err, out)
			}
			for _, name := range []string{"previews", "contacts_updated.csv", "sent_log.csv"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("check wrote %s", name)
				}
			}
		})
	}
}

func TestCLIInteractiveCampaign(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "contacts.csv", strings.ReplaceAll(mixedCSV, "Not contacted", "da_revisionare"))
	writeFixture(t, dir, "cv.txt", "attachment")
	// Invalid mode and negative limit are retried; keep other defaults and choose check.
	input := strings.Join([]string{"9", "1", "", "da_revisionare", "", "", "1", "-", "-", "", "-2", "2", "", "", "", "", "2", ""}, "\n")
	out, err := runCLI(t, dir, input, "--interactive", "--contacts", "contacts.csv", "--attachment", "cv.txt")
	if err != nil || !strings.Contains(out, "Valore non valido") || !strings.Contains(out, "it=1 | en=1") || !strings.Contains(out, "da_revisionare") {
		t.Fatalf("interactive CLI: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "previews")); !os.IsNotExist(err) {
		t.Fatal("interactive check wrote previews")
	}
	out, err = runCLI(t, dir, "", "--interactive", "--contacts", "contacts.csv", "--attachment", "cv.txt")
	if err == nil || !strings.Contains(out, "EOF") {
		t.Fatalf("EOF did not abort: %v\n%s", err, out)
	}
}

func TestWizardPreservesConfirmationInput(t *testing.T) {
	dir := t.TempDir()
	attachment := writeFixture(t, dir, "cv.txt", "attachment")
	opts := wizardOptions{to: "one@example.com", company: "Acme", language: "en", attachments: []string{attachment}, log: "sent_log.csv"}
	input := strings.Join([]string{"2", "", "", "it", "2", "", "", "Prima riga", "Seconda riga", "", "1", "-", "-", "", "-", "", "3", "1", ""}, "\n")
	reader := bufio.NewReader(strings.NewReader(input))
	if err := configureWizard(&console{in: reader, out: io.Discard}, &opts); err != nil {
		t.Fatal(err)
	}
	if !opts.send || opts.check || opts.audience != "technical" || opts.text != "Prima riga\nSeconda riga" || !strings.HasPrefix(opts.greeting, "Gentile") {
		t.Fatalf("unexpected single-email configuration: %+v", opts)
	}
	if !confirmSend(reader, 1) {
		t.Fatal("confirmation input lost after interactive setup")
	}
}

func TestUnifiedTemplateVariants(t *testing.T) {
	t.Setenv("MAIL_FROM_NAME", "Alex Example")
	t.Setenv("MAIL_FROM", "sender@example.com")
	t.Setenv("MAIL_SENDER_TITLE", "Software Developer")
	t.Setenv("MAIL_SENDER_WEBSITE", "www.example.com")
	for _, language := range []string{"it", "en"} {
		set, err := loadTemplates("templates/email_template_unificato.txt", "", "", language)
		if err != nil {
			t.Fatal(err)
		}
		for _, audience := range []string{"technical", "nontechnical"} {
			t.Run(language+"/"+audience, func(t *testing.T) {
				c, body, err := set.render(contact{Company: "Example", Audience: audience, PersonalizedText: "PERSONALIZED PARAGRAPH"})
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{c.Greeting, "PERSONALIZED PARAGRAPH", "Alex Example", "sender@example.com", "www.example.com", "WordPress"} {
					if !strings.Contains(body, want) {
						t.Errorf("missing %q in %s/%s", want, language, audience)
					}
				}
				isTechnical := audience == "technical"
				for _, technology := range []string{"PyTorch", "React Native", "ChromaDB", "Kubernetes", "Linguistic Linked Open Data"} {
					if strings.Contains(body, technology) != isTechnical {
						t.Errorf("wrong audience branch for %q", technology)
					}
				}
				if strings.Contains(body, "Cordiali saluti,") != (language == "it") || strings.Contains(body, "Best regards,") != (language == "en") {
					t.Error("wrong language branch")
				}
				if strings.Contains(body, "{{") || strings.Contains(body, "<no value>") {
					t.Fatal("unrendered placeholders")
				}
			})
		}
	}
}

func TestCLIAudienceSelection(t *testing.T) {
	templatePath, err := filepath.Abs("templates/email_template_unificato.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, column, value, fallback, want string
		wantError                           bool
	}{
		{"CSV wins", "Audience", "technical", "nontechnical", "technical=2 | nontechnical=0", false},
		{"Italian alias", "Pubblico", "non tecnico", "technical", "technical=0 | nontechnical=2", false},
		{"blank uses fallback", "Audience", "", "technical", "technical=2 | nontechnical=0", false},
		{"legacy defaults to nontechnical", "", "", "nontechnical", "technical=0 | nontechnical=2", false},
		{"invalid audience blocks", "Audience", "unknown", "nontechnical", "unsupported audience", true},
		{"invalid fallback blocks", "Audience", "technical", "unknown", "unsupported audience", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			content := mixedCSV
			if tc.column != "" {
				content = strings.Replace(content, "Language\n", "Language,"+tc.column+"\n", 1)
				content = strings.ReplaceAll(content, ",it\n", ",it,"+tc.value+"\n")
				content = strings.ReplaceAll(content, ",en\n", ",en,"+tc.value+"\n")
			}
			writeFixture(t, dir, "contacts.csv", content)
			writeFixture(t, dir, "cv.txt", "attachment")
			out, err := runCLI(t, dir, "", "--contacts", "contacts.csv", "--attachment", "cv.txt", "--template", templatePath, "--audience", tc.fallback, "--check")
			if (err != nil) != tc.wantError || !strings.Contains(out, tc.want) {
				t.Fatalf("CLI: %v\n%s", err, out)
			}
		})
	}
}
