package feedback

import "github.com/Ferousco-dev/mailx/internal/bounce"

// Classify maps one ParsedDSN's structured fields to a Kind. It follows
// RFC 3464 3.2 ("Action" is the field a DSN generator sets to categorize the
// outcome) and never looks at free text. See docs/design-v0.32.md "Bounce
// classification" for the conservative rule this encodes, mirrored from
// suppression.QualifiesHardBounce's synchronous rule:
//
//	Action: failed,  5.x.x  -> KindBouncePermanent (suppression-worthy only for
//	                            the narrow mailbox-invalidity codes, decided by
//	                            suppression.QualifiesAsyncHardBounce — NOT here)
//	Action: delayed, 4.x.x  -> KindBounceTemporary  (NEVER suppresses)
//	anything else            -> KindBounceUnknown    (NEVER suppresses: malformed,
//	                             missing Action/Status, "delivered"/"relayed"/
//	                             "expanded", or a Status class other than 4/5)
//
// A missing or unparseable Status does not default to permanent: an
// under-specified DSN is exactly the "uncertain correlation/diagnostic" case
// the milestone requires MailX to treat conservatively.
func Classify(d ParsedDSN) (Kind, bounce.EnhancedStatus) {
	status, err := bounce.ParseEnhancedStatus(d.Status)
	if err != nil {
		return KindBounceUnknown, bounce.EnhancedStatus{}
	}
	switch {
	case d.Action == ActionFailed && status.IsPermanent():
		return KindBouncePermanent, status
	case d.Action == ActionDelayed && status.IsTemporary():
		return KindBounceTemporary, status
	default:
		// Covers Action=delivered/relayed/expanded/unknown, and any
		// Action+Status combination that does not match one of the two rules
		// above (e.g. Action: failed with a 4.x.x status — internally
		// inconsistent, so treated as unknown rather than guessed at).
		return KindBounceUnknown, status
	}
}
