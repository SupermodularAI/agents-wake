package record

import (
	"reflect"
	"testing"
	"time"
)

func TestRecordContainsNoPlainStrings(t *testing.T) {
	typeOfRecord := reflect.TypeFor[Record]()
	for i := range typeOfRecord.NumField() {
		field := typeOfRecord.Field(i)
		if field.Type.Kind() == reflect.String && field.Type.Name() == "string" {
			t.Fatalf("%s is an unconstrained string field", field.Name)
		}
	}
}

func TestValidate(t *testing.T) {
	record := validRecord()
	if err := Validate(record); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	record.Name = "contains space"
	if err := Validate(record); err == nil {
		t.Fatal("Validate() accepted a name with whitespace")
	}
}

func TestOutcomeHasNoSuccessZeroValue(t *testing.T) {
	if Outcome("") == OutcomeOK {
		t.Fatal("zero Outcome must not mean success")
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	record := validRecord()
	first, err := Marshal(record)
	if err != nil {
		t.Fatalf("Marshal() first error = %v", err)
	}
	second, err := Marshal(record)
	if err != nil {
		t.Fatalf("Marshal() second error = %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("Marshal() differs: %s != %s", first, second)
	}
}

func TestDeriveEventID(t *testing.T) {
	harness := Identifier("claude-code")
	source := Identifier("source-event-1")
	first := DeriveEventID(harness, source)
	second := DeriveEventID(harness, source)
	if first != second {
		t.Fatal("DeriveEventID() is not deterministic")
	}
}

func validRecord() Record {
	outcome := OutcomeOK
	return Record{
		SchemaVersion: SchemaVersion,
		EventID:       DeriveEventID("claude-code", "source-event-1"),
		Timestamp:     time.Date(2026, time.August, 13, 12, 0, 0, 0, time.UTC),
		Harness:       "claude-code",
		SessionID:     "session-1",
		Repo:          "0123456789abcdef0123456789abcdef",
		Kind:          KindSkill,
		Name:          "review",
		Invoker:       InvokerModel,
		Outcome:       &outcome,
	}
}
