package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

func TestNewID(t *testing.T) {
	first, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first == second {
		t.Fatalf("unexpected IDs: %q and %q", first, second)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(first) {
		t.Fatalf("ID is not path-safe hexadecimal: %q", first)
	}
}

func TestNewIDConcurrent(t *testing.T) {
	const count = 64
	ids := make(chan string, count)
	errs := make(chan error, count)
	var group sync.WaitGroup
	for range count {
		group.Add(1)
		go func() {
			defer group.Done()
			id, err := NewID()
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	group.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seen := make(map[string]struct{}, count)
	for id := range ids {
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate ID generated: %q", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != count {
		t.Fatalf("generated %d IDs, want %d", len(seen), count)
	}
}

func TestNewMessageRecordKeepsSenderMessageIDSeparate(t *testing.T) {
	message := mail.Message{MessageID: "<sender@example.com>"}
	envelope := mail.Envelope{MailFrom: "<bounce@example.com>", Recipients: []string{"<recipient@example.com>"}}
	record, err := NewMessageRecord(envelope, message)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID == "" || record.ID == message.MessageID || record.Message.MessageID != message.MessageID || record.Envelope.MailFrom != envelope.MailFrom || record.ReceivedAt.IsZero() || record.ReceivedAt.Location() != time.UTC {
		t.Fatalf("MailX and sender identities were confused: %#v", record)
	}
}

func TestNewFileStoreInitializesMessagesDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mailx-data")
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if store.Root() != root || store.MessagesDir() != filepath.Join(root, messagesDir) {
		t.Fatalf("unexpected storage paths: root=%q messages=%q", store.Root(), store.MessagesDir())
	}
	info, err := os.Stat(store.MessagesDir())
	if err != nil || !info.IsDir() {
		t.Fatalf("messages directory was not initialized: info=%v err=%v", info, err)
	}
	if _, err := os.ReadDir(store.MessagesDir()); err != nil {
		t.Fatal(err)
	}
	if err := store.Initialize(); err != nil {
		t.Fatalf("initializing an existing directory failed: %v", err)
	}
}

func TestNewFileStoreUsesDefaultRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	store, err := NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	if store.Root() != DefaultRoot || store.MessagesDir() != filepath.Join(DefaultRoot, messagesDir) {
		t.Fatalf("unexpected default paths: %#v", store)
	}
	if info, err := os.Stat(store.MessagesDir()); err != nil || !info.IsDir() {
		t.Fatalf("default messages directory was not initialized: info=%v err=%v", info, err)
	}
}

func TestNewFileStoreReturnsInitializationError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(root); err == nil {
		t.Fatal("NewFileStore returned no error for a file storage root")
	}
}

func TestFileStoreSavePreservesRawMessage(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Hello\r\n\r\nHello Bob.\r\nSecond line.\r\n"
	record := testRecord(strings.Repeat("a", 32), raw)
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.MessagesDir(), record.ID, messageFile)
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, []byte(raw)) {
		t.Fatalf("stored raw message changed:\n got %q\nwant %q", stored, raw)
	}
	if _, err := os.Stat(filepath.Join(store.MessagesDir(), record.ID, attachmentsDir)); !os.IsNotExist(err) {
		t.Fatalf("attachment-free message unexpectedly has an attachments directory: %v", err)
	}
}

func TestFileStoreSavePreservesMIMERawMessage(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := "Content-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: text/plain\r\n\r\nHello\r\n--mailx\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"report.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--mailx--\r\n"
	record := testRecord(strings.Repeat("b", 32), raw)
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(store.MessagesDir(), record.ID, messageFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, []byte(raw)) {
		t.Fatal("MIME boundaries or encoded attachment content changed")
	}
}

func TestFileStoreSaveRejectsInvalidOrEmptyRecords(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "short", strings.Repeat("A", 32), "abc/def", "../" + strings.Repeat("a", 29), strings.Repeat("g", 32), strings.Repeat("a", 33)} {
		if err := store.Save(MessageRecord{ID: id, Message: mail.Message{Raw: "raw"}}); err == nil {
			t.Fatalf("Save accepted invalid ID %q", id)
		}
	}
	if err := store.Save(MessageRecord{ID: strings.Repeat("c", 32)}); err == nil {
		t.Fatal("Save accepted an empty raw message")
	}
	if err := store.Save(MessageRecord{ID: strings.Repeat("c", 32), Message: mail.Message{Raw: "raw"}}); err == nil {
		t.Fatal("Save accepted a record without a received timestamp")
	}
}

func TestFileStoreSaveDoesNotOverwrite(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord(strings.Repeat("d", 32), "first")
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(store.MessagesDir(), record.ID, metadataFile)
	originalMetadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	record.Message.Raw = "second"
	if err := store.Save(record); err == nil {
		t.Fatal("Save overwrote an existing record")
	}
	stored, err := os.ReadFile(filepath.Join(store.MessagesDir(), record.ID, messageFile))
	if err != nil || string(stored) != "first" {
		t.Fatalf("existing raw message changed: %q, err=%v", stored, err)
	}
	storedMetadata, err := os.ReadFile(metadataPath)
	if err != nil || !bytes.Equal(storedMetadata, originalMetadata) {
		t.Fatalf("existing metadata changed: %q, err=%v", storedMetadata, err)
	}
}

func TestFileStoreSaveConcurrent(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const count = 32
	var group sync.WaitGroup
	errs := make(chan error, count)
	for index := range count {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			errs <- store.Save(testRecord(fmt.Sprintf("%032x", index+1), fmt.Sprintf("message %d", index)))
		}(index)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for index := range count {
		path := filepath.Join(store.MessagesDir(), fmt.Sprintf("%032x", index+1), messageFile)
		if stored, err := os.ReadFile(path); err != nil || string(stored) != fmt.Sprintf("message %d", index) {
			t.Fatalf("concurrent save %d failed: %q, err=%v", index, stored, err)
		}
	}
}

func TestFileStoreSaveConcurrentSameID(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord(strings.Repeat("e", 32), "immutable")
	const attempts = 16
	results := make(chan error, attempts)
	var group sync.WaitGroup
	for range attempts {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- store.Save(record)
		}()
	}
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("same-ID saves succeeded %d times, want 1", successes)
	}
	stored, err := os.ReadFile(filepath.Join(store.MessagesDir(), record.ID, messageFile))
	if err != nil || string(stored) != record.Message.Raw {
		t.Fatalf("same-ID save corrupted raw message: %q, err=%v", stored, err)
	}
}

func TestFileStoreSaveReturnsFilesystemError(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.MessagesDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.MessagesDir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(testRecord(strings.Repeat("f", 32), "raw")); err == nil {
		t.Fatal("Save returned no error for an unusable messages path")
	}
}

func TestFileStoreSavePersistsDerivedMetadata(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receivedAt := time.Date(2026, time.September, 13, 12, 34, 56, 0, time.UTC)
	raw := "From: Newsletter <news@example.com>\r\nTo: User <user@example.com>\r\nSubject: raw-message-marker\r\n\r\nraw-body-marker\r\n"
	record := MessageRecord{
		ID:         strings.Repeat("1", 32),
		ReceivedAt: receivedAt,
		Envelope:   mail.Envelope{MailFrom: "<bounce@example.com>", Recipients: []string{"<user@example.com>", "<archive@example.com>"}},
		Message: mail.Message{
			From:                    "Newsletter <news@example.com>",
			To:                      []string{"User <user@example.com>"},
			Cc:                      []string{"Copy <copy@example.com>"},
			Bcc:                     []string{"Hidden <hidden@example.com>"},
			Subject:                 "Metadata test",
			Date:                    "Mon, 01 Jan 2024 12:00:00 +0000",
			MessageID:               "<sender-id@example.com>",
			MIMEVersion:             "1.0",
			ContentType:             "multipart/mixed; boundary=mailx",
			MediaType:               "multipart/mixed",
			Charset:                 "UTF-8",
			ContentTransferEncoding: "7bit",
			ContentDisposition:      "inline",
			Raw:                     raw,
			Body:                    "body-marker",
			TextBody:                "text-body-marker",
			HTMLBody:                "html-body-marker",
			Attachments: []mail.Attachment{
				{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte("attachment-secret-one")},
				{Filename: "notes.txt", ContentType: "text/plain", Content: []byte("attachment-secret-two")},
			},
		},
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(store.MessagesDir(), record.ID)
	storedRaw, err := os.ReadFile(filepath.Join(directory, messageFile))
	if err != nil || !bytes.Equal(storedRaw, []byte(raw)) {
		t.Fatalf("raw message was not preserved: %q, err=%v", storedRaw, err)
	}
	encodedMetadata, err := os.ReadFile(filepath.Join(directory, metadataFile))
	if err != nil {
		t.Fatal(err)
	}
	var metadata StoredMessageMetadata
	if err := json.Unmarshal(encodedMetadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.ID != record.ID || !metadata.ReceivedAt.Equal(receivedAt) || metadata.Envelope.MailFrom != "<bounce@example.com>" || len(metadata.Envelope.RcptTo) != 2 || metadata.Message.From != record.Message.From || metadata.Message.Subject != record.Message.Subject || metadata.Message.MessageID != record.Message.MessageID || metadata.Message.Date != record.Message.Date {
		t.Fatalf("unexpected stored metadata: %#v", metadata)
	}
	if metadata.MIME.ContentType != record.Message.ContentType || metadata.MIME.MediaType != record.Message.MediaType || metadata.MIME.Charset != record.Message.Charset || metadata.MIME.ContentTransferEncoding != record.Message.ContentTransferEncoding || len(metadata.Attachments) != 2 || metadata.Attachments[0].Filename != "report.pdf" || metadata.Attachments[0].Size != len(record.Message.Attachments[0].Content) || metadata.Attachments[0].StoredName != "0001.bin" || metadata.Attachments[1].Filename != "notes.txt" || metadata.Attachments[1].StoredName != "0002.bin" {
		t.Fatalf("unexpected MIME or attachment metadata: %#v", metadata)
	}
	for index, attachment := range record.Message.Attachments {
		stored, err := os.ReadFile(filepath.Join(directory, attachmentsDir, storedAttachmentName(index)))
		if err != nil || !bytes.Equal(stored, attachment.Content) {
			t.Fatalf("attachment %d was not persisted exactly: %q, err=%v", index, stored, err)
		}
	}
	for _, forbidden := range []string{"raw-message-marker", "raw-body-marker", "body-marker", "text-body-marker", "html-body-marker", "attachment-secret-one", "attachment-secret-two"} {
		if bytes.Contains(encodedMetadata, []byte(forbidden)) {
			t.Fatalf("metadata duplicated excluded content %q: %s", forbidden, encodedMetadata)
		}
	}
}

func TestFileStoreSavePersistsAttachmentsSafely(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord(strings.Repeat("2", 32), "Subject: attachments\r\n\r\nraw body\r\n")
	record.Message.Attachments = []mail.Attachment{
		{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte{0x25, 0x50, 0x44, 0x46, 0x00, 0xff}},
		{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte("duplicate filename")},
		{Filename: "../../etc/passwd", ContentType: "application/octet-stream", Content: []byte{}},
		{Filename: "/absolute/path.txt", ContentType: "text/plain", Content: []byte("absolute")},
		{Filename: "foo/bar.txt", ContentType: "text/plain", Content: []byte("slash")},
		{Filename: `foo\bar.txt`, ContentType: "text/plain", Content: []byte("backslash")},
		{Filename: "..", ContentType: "application/octet-stream", Content: []byte("dots")},
	}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(store.MessagesDir(), record.ID)
	attachmentDirectory := filepath.Join(directory, attachmentsDir)
	for index, attachment := range record.Message.Attachments {
		name := storedAttachmentName(index)
		path := filepath.Join(attachmentDirectory, name)
		relative, err := filepath.Rel(attachmentDirectory, path)
		if err != nil || relative != name || filepath.Dir(relative) != "." {
			t.Fatalf("attachment path escaped its directory: path=%q relative=%q err=%v", path, relative, err)
		}
		stored, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(stored, attachment.Content) {
			t.Fatalf("attachment %d bytes changed: %q, err=%v", index, stored, err)
		}
	}
	encodedMetadata, err := os.ReadFile(filepath.Join(directory, metadataFile))
	if err != nil {
		t.Fatal(err)
	}
	var metadata StoredMessageMetadata
	if err := json.Unmarshal(encodedMetadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Attachments) != len(record.Message.Attachments) || metadata.Attachments[0].StoredName != "0001.bin" || metadata.Attachments[1].StoredName != "0002.bin" || metadata.Attachments[2].Filename != "../../etc/passwd" || metadata.Attachments[2].Size != 0 || metadata.Attachments[3].Filename != "/absolute/path.txt" {
		t.Fatalf("unexpected attachment metadata: %#v", metadata.Attachments)
	}
}

func TestFileStoreSavePersistsParserDecodedAttachments(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=mailx\r\n\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\n\r\nVisible text\r\n" +
		"--mailx\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"encoded.bin\"\r\nContent-Transfer-Encoding: base64\r\n\r\nAP8B\r\n" +
		"--mailx\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"notes.txt\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHello=20MailX\r\n" +
		"--mailx--\r\n"
	message, err := mail.ParseMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord(strings.Repeat("3", 32), raw)
	record.Message = message
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(store.MessagesDir(), record.ID, attachmentsDir)
	if stored, err := os.ReadFile(filepath.Join(directory, "0001.bin")); err != nil || !bytes.Equal(stored, []byte{0x00, 0xff, 0x01}) {
		t.Fatalf("Base64 attachment was not decoded/persisted: %q, err=%v", stored, err)
	}
	if stored, err := os.ReadFile(filepath.Join(directory, "0002.bin")); err != nil || string(stored) != "Hello MailX" {
		t.Fatalf("quoted-printable attachment was not decoded/persisted: %q, err=%v", stored, err)
	}
}

func TestFileStoreSaveCleansUpAfterAttachmentFailure(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.writeAttachment = func(path string, content []byte) error {
		calls++
		if calls == 2 {
			return errors.New("simulated attachment write failure")
		}
		return writeBytesExclusive(path, content)
	}
	record := testRecord(strings.Repeat("4", 32), "Subject: failure\r\n\r\nbody")
	record.Message.Attachments = []mail.Attachment{
		{Filename: "one.bin", Content: []byte("one")},
		{Filename: "two.bin", Content: []byte("two")},
	}
	if err := store.Save(record); err == nil {
		t.Fatal("Save returned no error after attachment write failure")
	}
	if _, err := os.Stat(filepath.Join(store.MessagesDir(), record.ID)); !os.IsNotExist(err) {
		t.Fatalf("partial message directory remained after attachment failure: %v", err)
	}
}

func TestFileStoreLoadReadsExactRawMessageAndMetadata(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord(strings.Repeat("5", 32), "From: sender@example.com\r\n\r\nexact raw\x00bytes\r\n")
	record.Envelope = mail.Envelope{MailFrom: "<sender@example.com>", Recipients: []string{"<recipient@example.com>"}}
	record.Message.Subject = "Stored subject"
	record.Message.Attachments = []mail.Attachment{{Filename: "report.pdf", ContentType: "application/pdf", Content: []byte("PDF")}}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Raw, []byte(record.Message.Raw)) {
		t.Fatalf("loaded raw message changed: %q", loaded.Raw)
	}
	if loaded.Metadata.ID != record.ID || !loaded.Metadata.ReceivedAt.Equal(record.ReceivedAt) || loaded.Metadata.Envelope.MailFrom != record.Envelope.MailFrom || loaded.Metadata.Message.Subject != record.Message.Subject {
		t.Fatalf("unexpected loaded metadata: %#v", loaded.Metadata)
	}
	if len(loaded.Metadata.Attachments) != 1 || loaded.Metadata.Attachments[0].Filename != "report.pdf" || loaded.Metadata.Attachments[0].ContentType != "application/pdf" || loaded.Metadata.Attachments[0].Size != 3 || loaded.Metadata.Attachments[0].StoredName != "0001.bin" {
		t.Fatalf("unexpected loaded attachment metadata: %#v", loaded.Metadata.Attachments)
	}
	if err := os.Remove(filepath.Join(store.MessagesDir(), record.ID, attachmentsDir, "0001.bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(record.ID); err != nil {
		t.Fatalf("Load should not read attachment bytes: %v", err)
	}
}

func TestFileStoreLoadRejectsInvalidIDAndCorruptMetadata(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("../" + strings.Repeat("a", 29)); err == nil {
		t.Fatal("Load accepted an invalid ID")
	}
	record := testRecord(strings.Repeat("6", 32), "raw")
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(store.MessagesDir(), record.ID, metadataFile)
	if err := os.WriteFile(metadataPath, []byte("not JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(record.ID); err == nil {
		t.Fatal("Load accepted invalid JSON metadata")
	}

	metadata := newMetadata(record)
	metadata.ID = strings.Repeat("7", 32)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadataPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(record.ID); err == nil {
		t.Fatal("Load accepted metadata with a mismatched ID")
	}
}

func TestFileStoreLoadRejectsInvalidAttachmentMetadata(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testRecord(strings.Repeat("8", 32), "raw")
	record.Message.Attachments = []mail.Attachment{{Filename: "report.pdf", Content: []byte("PDF")}}
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	metadata := newMetadata(record)
	metadata.Attachments[0].StoredName = "../../outside"
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.MessagesDir(), record.ID, metadataFile), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(record.ID); err == nil {
		t.Fatal("Load accepted an unsafe attachment stored name")
	}
}

func TestFileStoreLoadReturnsErrorsForMissingFiles(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missingMetadata := testRecord(strings.Repeat("9", 32), "raw")
	missingRaw := testRecord(strings.Repeat("c", 32), "raw")
	for _, record := range []MessageRecord{missingMetadata, missingRaw} {
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(store.MessagesDir(), missingMetadata.ID, metadataFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(missingMetadata.ID); err == nil {
		t.Fatal("Load accepted a record without metadata.json")
	}
	if err := os.Remove(filepath.Join(store.MessagesDir(), missingRaw.ID, messageFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(missingRaw.ID); err == nil {
		t.Fatal("Load accepted a record without message.eml")
	}
}

func TestFileStoreListReadsStoredMetadataInIDOrder(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := testRecord(strings.Repeat("a", 32), "first")
	first.Message.Subject = "first subject"
	second := testRecord(strings.Repeat("b", 32), "second")
	second.Message.Subject = "second subject"
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.MessagesDir(), "notes.txt"), []byte("not a message"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.MessagesDir(), "scratch"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.MessagesDir(), first.ID, messageFile)); err != nil {
		t.Fatal(err)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].ID != first.ID || listed[0].Message.Subject != first.Message.Subject || listed[1].ID != second.ID || listed[1].Message.Subject != second.Message.Subject {
		t.Fatalf("unexpected listed messages: %#v", listed)
	}
}

func TestFileStoreRestartPreservesCompleteMIMERecord(t *testing.T) {
	root := t.TempDir()
	message, err := mail.ParseMessage("From: Sender <sender@example.com>\r\nTo: Recipient <recipient@example.com>\r\nSubject: restart test\r\nContent-Type: multipart/mixed; boundary=mailx\r\n\r\n--mailx\r\nContent-Type: multipart/alternative; boundary=alternative\r\n\r\n--alternative\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nPlain body\r\n--alternative\r\nContent-Type: text/html; charset=UTF-8\r\n\r\n<p>HTML body</p>\r\n--alternative--\r\n--mailx\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=report.bin\r\nContent-Transfer-Encoding: base64\r\n\r\nAP8B\r\n--mailx--\r\n")
	if err != nil {
		t.Fatal(err)
	}
	firstStore, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewMessageRecord(mail.Envelope{MailFrom: "<bounce@example.com>", Recipients: []string{"<recipient@example.com>"}}, message)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Save(record); err != nil {
		t.Fatal(err)
	}

	secondStore, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := secondStore.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != record.ID || !listed[0].ReceivedAt.Equal(record.ReceivedAt) || listed[0].Envelope.MailFrom != record.Envelope.MailFrom || listed[0].Message.Subject != message.Subject || listed[0].MIME.MediaType != "multipart/mixed" || len(listed[0].Attachments) != 1 {
		t.Fatalf("unexpected restarted listing: %#v", listed)
	}
	loaded, err := secondStore.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Raw, []byte(message.Raw)) || loaded.Metadata.ID != record.ID || loaded.Metadata.Attachments[0].StoredName != "0001.bin" || loaded.Metadata.Attachments[0].Size != 3 {
		t.Fatalf("unexpected restarted record: %#v", loaded)
	}
	attachment, err := os.ReadFile(filepath.Join(secondStore.MessagesDir(), record.ID, attachmentsDir, "0001.bin"))
	if err != nil || !bytes.Equal(attachment, []byte{0x00, 0xff, 0x01}) {
		t.Fatalf("unexpected restarted attachment: %q, err=%v", attachment, err)
	}
}

func TestFileStoreReadRejectsSymlinks(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	metadataLink := testRecord(strings.Repeat("d", 32), "metadata link")
	rawLink := testRecord(strings.Repeat("e", 32), "raw link")
	directoryLink := testRecord(strings.Repeat("f", 32), "directory link")
	for _, record := range []MessageRecord{metadataLink, rawLink, directoryLink} {
		if err := store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	metadataPath := filepath.Join(store.MessagesDir(), metadataLink.ID, metadataFile)
	if err := os.Remove(metadataPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store.MessagesDir(), rawLink.ID, metadataFile), metadataPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(metadataLink.ID); err == nil {
		t.Fatal("Load followed a metadata symlink")
	}
	rawPath := filepath.Join(store.MessagesDir(), rawLink.ID, messageFile)
	if err := os.Remove(rawPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store.MessagesDir(), metadataLink.ID, metadataFile), rawPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(rawLink.ID); err == nil {
		t.Fatal("Load followed a raw-message symlink")
	}
	directoryPath := filepath.Join(store.MessagesDir(), directoryLink.ID)
	if err := os.RemoveAll(directoryPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store.MessagesDir(), rawLink.ID), directoryPath); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil {
		t.Fatal("List followed a message-directory symlink")
	}
}

func TestFileStoreSaveConcurrentMessagesRemainIndependent(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const count = 16
	records := make([]MessageRecord, count)
	var group sync.WaitGroup
	errs := make(chan error, count)
	for index := range records {
		records[index] = testRecord(fmt.Sprintf("%032x", index+100), fmt.Sprintf("raw message %d", index))
		records[index].Message.Attachments = []mail.Attachment{{Filename: "same-name.bin", ContentType: "application/octet-stream", Content: []byte(fmt.Sprintf("attachment %d", index))}}
		group.Add(1)
		go func(record MessageRecord) {
			defer group.Done()
			errs <- store.Save(record)
		}(records[index])
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for index, record := range records {
		loaded, err := store.Load(record.ID)
		if err != nil || !bytes.Equal(loaded.Raw, []byte(record.Message.Raw)) || len(loaded.Metadata.Attachments) != 1 || loaded.Metadata.Attachments[0].StoredName != "0001.bin" {
			t.Fatalf("record %d was corrupted: %#v, err=%v", index, loaded, err)
		}
		attachment, err := os.ReadFile(filepath.Join(store.MessagesDir(), record.ID, attachmentsDir, "0001.bin"))
		if err != nil || !bytes.Equal(attachment, record.Message.Attachments[0].Content) {
			t.Fatalf("record %d attachment was corrupted: %q, err=%v", index, attachment, err)
		}
	}
}

func testRecord(id, raw string) MessageRecord {
	return MessageRecord{
		ID:         id,
		ReceivedAt: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		Message:    mail.Message{Raw: raw},
	}
}

// TestFileStoreSaveRepairsCrashPartialRecord proves the PR review fix: if a
// prior Save crashed after creating the message directory but before
// writing message.eml/metadata.json, a later Save for the SAME id must not
// treat "directory already exists" as a completed prior save (ErrRecordExists)
// — that would let a caller durably reference a record the worker can never
// load (permanently unsendable, per the finding). Save must instead detect
// the incompleteness, repair it, and write a complete record.
func TestFileStoreSaveRepairsCrashPartialRecord(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("c", 32)

	// Simulate the crash: only the message directory exists, no files in it
	// (exactly what a crash between os.Mkdir and the raw-message write
	// leaves behind) — and old enough (older than repairGracePeriod) to be
	// distinguishable from a genuinely concurrent in-flight writer, which
	// Save must NOT repair (see TestFileStoreSaveConcurrentSameID).
	dir := filepath.Join(store.MessagesDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-repairGracePeriod - time.Second)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	record := testRecord(id, "raw content")
	if err := store.Save(record); err != nil {
		t.Fatalf("Save must repair a crash-partial record, not fail: %v", err)
	}
	loaded, err := store.Load(id)
	if err != nil || string(loaded.Raw) != "raw content" {
		t.Fatalf("repaired record must be fully loadable: %+v %v", loaded, err)
	}
}

// A genuinely COMPLETE existing record must still return ErrRecordExists
// (the deliberate idempotent-retry path) — the repair above must not kick
// in for a real prior successful save.
func TestFileStoreSaveStillReportsExistsForCompleteRecord(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("d", 32)
	record := testRecord(id, "raw content")
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(record); !errors.Is(err, ErrRecordExists) {
		t.Fatalf("Save on a genuinely complete record = %v, want ErrRecordExists", err)
	}
	// And the original content must survive untouched.
	loaded, err := store.Load(id)
	if err != nil || string(loaded.Raw) != "raw content" {
		t.Fatalf("complete record must not be repaired/overwritten: %+v %v", loaded, err)
	}
}
