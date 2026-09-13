// Package storage contains MailX's local persistence foundation.
package storage

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

const (
	// DefaultRoot is the portable development location for MailX data.
	DefaultRoot    = "data"
	messagesDir    = "messages"
	messageFile    = "message.eml"
	metadataFile   = "metadata.json"
	attachmentsDir = "attachments"
)

// MessageRecord gives a parsed message a MailX-owned local identity. ID is
// separate from Message.MessageID, which is untrusted email metadata.
type MessageRecord struct {
	ID         string
	ReceivedAt time.Time
	Envelope   mail.Envelope
	Message    mail.Message
}

// StoredMessage is a message read from MailX's local persistence layout. Raw
// contains the exact bytes from message.eml. Attachments are represented by
// metadata only so loading a message does not also load potentially large
// attachment files into memory.
type StoredMessage struct {
	Metadata StoredMessageMetadata
	Raw      []byte
}

// StoredMessageMetadata is the derived, indexable metadata saved alongside a
// message's raw email.
type StoredMessageMetadata struct {
	ID          string             `json:"id"`
	ReceivedAt  time.Time          `json:"received_at"`
	Envelope    StoredEnvelope     `json:"envelope"`
	Message     StoredHeaders      `json:"message"`
	MIME        StoredMIME         `json:"mime"`
	Attachments []StoredAttachment `json:"attachments"`
}

// StoredEnvelope is the SMTP envelope recorded when MailX received a message.
type StoredEnvelope struct {
	MailFrom string   `json:"mail_from"`
	RcptTo   []string `json:"rcpt_to"`
}

// StoredHeaders contains parsed Internet-message headers saved as metadata.
type StoredHeaders struct {
	From      string   `json:"from"`
	To        []string `json:"to"`
	Cc        []string `json:"cc"`
	Bcc       []string `json:"bcc"`
	Subject   string   `json:"subject"`
	Date      string   `json:"date"`
	MessageID string   `json:"message_id"`
}

// StoredMIME contains parsed MIME metadata saved with a message.
type StoredMIME struct {
	Version                 string `json:"version"`
	ContentType             string `json:"content_type"`
	MediaType               string `json:"media_type"`
	Charset                 string `json:"charset"`
	ContentTransferEncoding string `json:"content_transfer_encoding"`
	ContentDisposition      string `json:"content_disposition"`
}

// StoredAttachment describes an attachment file controlled by MailX.
type StoredAttachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	StoredName  string `json:"stored_name"`
}

// NewMessageRecord creates a locally identifiable record for a parsed email.
func NewMessageRecord(envelope mail.Envelope, message mail.Message) (MessageRecord, error) {
	id, err := NewID()
	if err != nil {
		return MessageRecord{}, err
	}
	return MessageRecord{
		ID:         id,
		ReceivedAt: time.Now().UTC(),
		Envelope:   envelope,
		Message:    message,
	}, nil
}

// NewID returns a path-safe, locally generated MailX message identifier.
func NewID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate MailX message ID: %w", err)
	}
	return hex.EncodeToString(bytes), nil
}

// FileStore describes MailX's local filesystem layout.
type FileStore struct {
	root            string
	writeAttachment func(string, []byte) error
}

// NewFileStore creates a store rooted at root and initializes its messages
// directory. An empty root uses the portable development default, ./data.
func NewFileStore(root string) (*FileStore, error) {
	if root == "" {
		root = DefaultRoot
	}
	store := &FileStore{root: root, writeAttachment: writeBytesExclusive}
	if err := store.Initialize(); err != nil {
		return nil, err
	}
	return store, nil
}

// Root returns the configured storage root.
func (s *FileStore) Root() string {
	return s.root
}

// MessagesDir returns the future home of per-message storage directories.
func (s *FileStore) MessagesDir() string {
	return filepath.Join(s.root, messagesDir)
}

// Initialize safely creates the storage root and messages directory. It does
// not create message directories or write message data.
func (s *FileStore) Initialize() error {
	if err := os.MkdirAll(s.MessagesDir(), 0o700); err != nil {
		return fmt.Errorf("initialize MailX storage at %q: %w", s.MessagesDir(), err)
	}
	return nil
}

// Save persists a record's exact raw message and its derived metadata. A record
// ID may be saved only once.
func (s *FileStore) Save(record MessageRecord) error {
	if err := validateID(record.ID); err != nil {
		return err
	}
	if record.Message.Raw == "" {
		return fmt.Errorf("save message %q: raw message is empty", record.ID)
	}
	if record.ReceivedAt.IsZero() {
		return fmt.Errorf("save message %q: received timestamp is empty", record.ID)
	}
	if err := s.Initialize(); err != nil {
		return err
	}

	messageDir := filepath.Join(s.MessagesDir(), record.ID)
	if err := os.Mkdir(messageDir, 0o700); err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("save message %q: record already exists", record.ID)
		}
		return fmt.Errorf("create message directory %q: %w", messageDir, err)
	}

	rawPath := filepath.Join(messageDir, messageFile)
	if err := writeStringExclusive(rawPath, record.Message.Raw); err != nil {
		s.cleanupFailedSave(messageDir)
		return fmt.Errorf("write raw message %q: %w", rawPath, err)
	}

	if len(record.Message.Attachments) > 0 {
		attachmentDirectory := filepath.Join(messageDir, attachmentsDir)
		if err := os.Mkdir(attachmentDirectory, 0o700); err != nil {
			s.cleanupFailedSave(messageDir)
			return fmt.Errorf("create attachments directory %q: %w", attachmentDirectory, err)
		}
		for index, attachment := range record.Message.Attachments {
			path := filepath.Join(attachmentDirectory, storedAttachmentName(index))
			if err := s.writeAttachmentFile(path, attachment.Content); err != nil {
				s.cleanupFailedSave(messageDir)
				return fmt.Errorf("write attachment %q: %w", path, err)
			}
		}
	}

	metadata, err := json.MarshalIndent(newMetadata(record), "", "  ")
	if err != nil {
		s.cleanupFailedSave(messageDir)
		return fmt.Errorf("marshal metadata for message %q: %w", record.ID, err)
	}
	metadataPath := filepath.Join(messageDir, metadataFile)
	if err := writeBytesExclusive(metadataPath, metadata); err != nil {
		s.cleanupFailedSave(messageDir)
		return fmt.Errorf("write metadata %q: %w", metadataPath, err)
	}
	return nil
}

// Load reads one persisted message by its MailX-owned ID. The raw message is
// read from message.eml; all other returned information comes from
// metadata.json.
func (s *FileStore) Load(id string) (StoredMessage, error) {
	if err := validateID(id); err != nil {
		return StoredMessage{}, err
	}
	metadata, err := s.loadMetadata(id)
	if err != nil {
		return StoredMessage{}, err
	}
	rawPath := filepath.Join(s.MessagesDir(), id, messageFile)
	raw, err := readRegularFile(rawPath)
	if err != nil {
		return StoredMessage{}, fmt.Errorf("read raw message %q: %w", rawPath, err)
	}
	return StoredMessage{Metadata: metadata, Raw: raw}, nil
}

// List returns metadata for every persisted message. Entries are ordered by
// their MailX IDs, matching os.ReadDir's lexical directory order.
func (s *FileStore) List() ([]StoredMessageMetadata, error) {
	entries, err := os.ReadDir(s.MessagesDir())
	if err != nil {
		return nil, fmt.Errorf("list messages in %q: %w", s.MessagesDir(), err)
	}
	messages := make([]StoredMessageMetadata, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("list messages: message directory %q is a symlink", entry.Name())
		}
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if err := validateID(id); err != nil {
			return nil, fmt.Errorf("list messages: invalid message directory %q: %w", id, err)
		}
		metadata, err := s.loadMetadata(id)
		if err != nil {
			return nil, err
		}
		messages = append(messages, metadata)
	}
	return messages, nil
}

func (s *FileStore) loadMetadata(id string) (StoredMessageMetadata, error) {
	messageDirectory := filepath.Join(s.MessagesDir(), id)
	if err := validateMessageDirectory(messageDirectory); err != nil {
		return StoredMessageMetadata{}, err
	}
	metadataPath := filepath.Join(s.MessagesDir(), id, metadataFile)
	encoded, err := readRegularFile(metadataPath)
	if err != nil {
		return StoredMessageMetadata{}, fmt.Errorf("read metadata %q: %w", metadataPath, err)
	}
	var metadata StoredMessageMetadata
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return StoredMessageMetadata{}, fmt.Errorf("decode metadata %q: %w", metadataPath, err)
	}
	if err := validateMetadata(id, metadata); err != nil {
		return StoredMessageMetadata{}, fmt.Errorf("validate metadata %q: %w", metadataPath, err)
	}
	return metadata, nil
}

func validateMessageDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect message directory %q: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("message directory %q is not a directory", path)
	}
	return nil
}

func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("path is not a regular file")
	}
	return os.ReadFile(path)
}

func writeStringExclusive(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := io.WriteString(file, content)
	closeErr := file.Close()
	if closeErr != nil {
		return closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	if written != len(content) {
		return io.ErrShortWrite
	}
	return nil
}

func writeBytesExclusive(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(content)
	closeErr := file.Close()
	if closeErr != nil {
		return closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	if written != len(content) {
		return io.ErrShortWrite
	}
	return nil
}

func (s *FileStore) writeAttachmentFile(path string, content []byte) error {
	if s.writeAttachment == nil {
		return writeBytesExclusive(path, content)
	}
	return s.writeAttachment(path, content)
}

func (s *FileStore) cleanupFailedSave(messageDir string) {
	_ = os.RemoveAll(messageDir)
}

func newMetadata(record MessageRecord) StoredMessageMetadata {
	metadata := StoredMessageMetadata{
		ID:         record.ID,
		ReceivedAt: record.ReceivedAt,
		Envelope: StoredEnvelope{
			MailFrom: record.Envelope.MailFrom,
			RcptTo:   record.Envelope.Recipients,
		},
		Message: StoredHeaders{
			From:      record.Message.From,
			To:        record.Message.To,
			Cc:        record.Message.Cc,
			Bcc:       record.Message.Bcc,
			Subject:   record.Message.Subject,
			Date:      record.Message.Date,
			MessageID: record.Message.MessageID,
		},
		MIME: StoredMIME{
			Version:                 record.Message.MIMEVersion,
			ContentType:             record.Message.ContentType,
			MediaType:               record.Message.MediaType,
			Charset:                 record.Message.Charset,
			ContentTransferEncoding: record.Message.ContentTransferEncoding,
			ContentDisposition:      record.Message.ContentDisposition,
		},
		Attachments: make([]StoredAttachment, 0, len(record.Message.Attachments)),
	}
	for index, attachment := range record.Message.Attachments {
		metadata.Attachments = append(metadata.Attachments, StoredAttachment{
			Filename:    attachment.Filename,
			ContentType: attachment.ContentType,
			Size:        len(attachment.Content),
			StoredName:  storedAttachmentName(index),
		})
	}
	return metadata
}

func storedAttachmentName(index int) string {
	return fmt.Sprintf("%04d.bin", index+1)
}

func validateMetadata(id string, metadata StoredMessageMetadata) error {
	if err := validateID(metadata.ID); err != nil {
		return fmt.Errorf("metadata ID: %w", err)
	}
	if metadata.ID != id {
		return fmt.Errorf("metadata ID %q does not match requested ID %q", metadata.ID, id)
	}
	if metadata.ReceivedAt.IsZero() {
		return fmt.Errorf("received timestamp is empty")
	}
	for index, attachment := range metadata.Attachments {
		if attachment.Size < 0 {
			return fmt.Errorf("attachment %d has a negative size", index+1)
		}
		if attachment.StoredName != storedAttachmentName(index) {
			return fmt.Errorf("attachment %d has invalid stored name %q", index+1, attachment.StoredName)
		}
	}
	return nil
}

func validateID(id string) error {
	if len(id) != 32 {
		return fmt.Errorf("invalid MailX message ID %q", id)
	}
	for _, character := range id {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return fmt.Errorf("invalid MailX message ID %q", id)
		}
	}
	return nil
}
