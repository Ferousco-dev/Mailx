package bounce

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Ferousco-dev/mailx/internal/delivery"
	"github.com/Ferousco-dev/mailx/internal/retry"
)

func TestParseEnhancedStatus(t *testing.T) {
	tests := []struct {
		value string
		want  EnhancedStatus
	}{
		{value: "5.1.1", want: EnhancedStatus{Class: 5, Subject: 1, Detail: 1}},
		{value: "4.2.2", want: EnhancedStatus{Class: 4, Subject: 2, Detail: 2}},
		{value: "2.0.0", want: EnhancedStatus{Class: 2, Subject: 0, Detail: 0}},
		{value: "5.999.999", want: EnhancedStatus{Class: 5, Subject: 999, Detail: 999}},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got, err := ParseEnhancedStatus(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want || !got.Valid() || got.String() != test.value {
				t.Fatalf("ParseEnhancedStatus(%q) = %+v", test.value, got)
			}
			roundTrip, err := ParseEnhancedStatus(got.String())
			if err != nil || roundTrip != got {
				t.Fatalf("round trip = %+v, %v", roundTrip, err)
			}
		})
	}
}

func TestParseEnhancedStatusRejectsMalformedValues(t *testing.T) {
	values := []string{
		"", "5", "5.1", "5.1.", ".1.1", "5..1", "5.1.1.0",
		"x.1.1", "3.1.1", "6.1.1", "5.-1.1", "5.1.-1",
		"5.000.1", "5.01.1", "5.1.01", "5.1000.1", "5.1.1000",
		"999999999999999999.1.1", "5.999999999999999999.1",
		" 5.1.1", "5.1.1 ", "5. 1.1", "5.1.1 extra", "5.1.1\ttext",
	}
	for _, value := range values {
		t.Run(value, func(t *testing.T) {
			got, err := ParseEnhancedStatus(value)
			if got != (EnhancedStatus{}) || !errors.Is(err, ErrInvalidEnhancedStatus) {
				t.Fatalf("ParseEnhancedStatus(%q) = %+v, %v", value, got, err)
			}
		})
	}
}

func TestEnhancedStatusClassHelpersAndCategories(t *testing.T) {
	tests := []struct {
		value     string
		success   bool
		temporary bool
		permanent bool
		category  StatusCategory
	}{
		{value: "2.0.0", success: true, category: CategoryUnknown},
		{value: "4.1.0", temporary: true, category: CategoryAddress},
		{value: "5.2.2", permanent: true, category: CategoryMailbox},
		{value: "5.3.0", permanent: true, category: CategorySystem},
		{value: "4.4.1", temporary: true, category: CategoryNetwork},
		{value: "5.5.2", permanent: true, category: CategoryProtocol},
		{value: "5.6.1", permanent: true, category: CategoryContent},
		{value: "5.7.1", permanent: true, category: CategoryPolicy},
		{value: "5.8.1", permanent: true, category: CategoryUnknown},
	}
	for _, test := range tests {
		status, err := ParseEnhancedStatus(test.value)
		if err != nil {
			t.Fatal(err)
		}
		if status.IsSuccess() != test.success || status.IsTemporary() != test.temporary ||
			status.IsPermanent() != test.permanent || status.Category() != test.category {
			t.Fatalf("status %s helpers/category are incorrect", test.value)
		}
	}

	var missing EnhancedStatus
	if missing.Valid() || missing.String() != "" || missing.IsSuccess() || missing.IsTemporary() || missing.IsPermanent() || missing.Category() != CategoryUnknown {
		t.Fatalf("zero enhanced status is not a safe missing value: %+v", missing)
	}
}

func TestClassifyEnhancedStatusGracefulDegradation(t *testing.T) {
	tests := []struct {
		name      string
		result    delivery.Result
		status    retry.LifecycleStatus
		wantClass FailureClass
		wantRaw   string
		wantCode  string
	}{
		{
			name: "permanent valid",
			result: delivery.Result{
				Kind: delivery.KindTransferPermanent, FinalCode: 550, EnhancedStatus: "5.1.1",
			},
			status: retry.StatusFailed, wantClass: FailurePermanentDelivery, wantRaw: "5.1.1", wantCode: "5.1.1",
		},
		{
			name: "exhausted temporary valid",
			result: delivery.Result{
				Kind: delivery.KindTransferTemporary, FinalCode: 451, EnhancedStatus: "4.4.1",
			},
			status: retry.StatusExhausted, wantClass: FailureRetryExhausted, wantRaw: "4.4.1", wantCode: "4.4.1",
		},
		{
			name: "missing",
			result: delivery.Result{
				Kind: delivery.KindTransferPermanent, FinalCode: 550,
			},
			status: retry.StatusFailed, wantClass: FailurePermanentDelivery,
		},
		{
			name: "malformed",
			result: delivery.Result{
				Kind: delivery.KindTransferPermanent, FinalCode: 550, EnhancedStatus: "5.999999999999.x",
			},
			status: retry.StatusFailed, wantClass: FailurePermanentDelivery, wantRaw: "5.999999999999.x",
		},
		{
			name: "permanent SMTP conflicts with temporary enhanced",
			result: delivery.Result{
				Kind: delivery.KindTransferPermanent, FinalCode: 550, EnhancedStatus: "4.2.0",
			},
			status: retry.StatusFailed, wantClass: FailurePermanentDelivery, wantRaw: "4.2.0", wantCode: "4.2.0",
		},
		{
			name: "exhausted temporary conflicts with permanent enhanced",
			result: delivery.Result{
				Kind: delivery.KindTransferTemporary, FinalCode: 451, EnhancedStatus: "5.1.1",
			},
			status: retry.StatusExhausted, wantClass: FailureRetryExhausted, wantRaw: "5.1.1", wantCode: "5.1.1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := stateWith(t, test.result, errors.New("delivery failure"))
			before := state.History()
			failure, err := Classify(state, test.status)
			if err != nil {
				t.Fatal(err)
			}
			if failure.Class != test.wantClass || failure.SMTPCode != test.result.FinalCode || failure.EnhancedStatusRaw != test.wantRaw {
				t.Fatalf("failure = %+v", failure)
			}
			if test.wantCode == "" {
				if failure.EnhancedStatus != nil {
					t.Fatalf("structured status = %+v, want nil", failure.EnhancedStatus)
				}
			} else if failure.EnhancedStatus == nil || failure.EnhancedStatus.String() != test.wantCode {
				t.Fatalf("structured status = %+v, want %q", failure.EnhancedStatus, test.wantCode)
			}
			if after := state.History(); !reflect.DeepEqual(after, before) {
				t.Fatalf("classification mutated state")
			}
		})
	}
}

func TestAcceptedEnhancedStatusNeverProducesBounce(t *testing.T) {
	for _, enhanced := range []string{"2.0.0", "malformed"} {
		state := stateWith(t, delivery.Result{
			Kind: delivery.KindAccepted, Accepted: true, FinalCode: 250, EnhancedStatus: enhanced,
		}, nil)
		if _, err := Classify(state, retry.StatusSucceeded); !errors.Is(err, ErrDeliverySucceeded) {
			t.Fatalf("accepted enhanced status %q error = %v", enhanced, err)
		}
	}
}

func FuzzParseEnhancedStatus(f *testing.F) {
	for _, seed := range []string{"5.1.1", "4.2.2", "2.0.0", "", "5.01.1", "5.999.999", "5.1.1 extra"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		status, err := ParseEnhancedStatus(value)
		if err != nil {
			if status != (EnhancedStatus{}) {
				t.Fatalf("failed parse returned nonzero value: %+v", status)
			}
			return
		}
		if !status.Valid() || status.String() == "" || (status.Class != 2 && status.Class != 4 && status.Class != 5) {
			t.Fatalf("successful parse returned invalid value: %+v", status)
		}
		roundTrip, err := ParseEnhancedStatus(status.String())
		if err != nil || roundTrip != status {
			t.Fatalf("round trip = %+v, %v; want %+v", roundTrip, err, status)
		}
	})
}
