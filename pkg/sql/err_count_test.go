// Copyright 2016 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql_test

import (
	"context"
	gosql "database/sql"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/server/telemetry"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/testutils/serverutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/errors"
	"github.com/lib/pq"
)

func TestErrorCounts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	telemetry.GetFeatureCounts(telemetry.Raw, telemetry.ResetCounts)

	params, _ := createTestServerParamsAllowTenants()
	s, db, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	count1 := telemetry.GetRawFeatureCounts()["errorcodes."+pgcode.Syntax.String()]

	_, err := db.Query("SELECT 1+")
	if err == nil {
		t.Fatal("expected error, got no error")
	}

	count2 := telemetry.GetRawFeatureCounts()["errorcodes."+pgcode.Syntax.String()]

	if count2-count1 != 1 {
		t.Fatalf("expected 1 syntax error, got %d", count2-count1)
	}

	rows, err := db.Query(`SHOW SYNTAX 'SELECT 1+'`)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	for rows.Next() {
		// Do nothing. SHOW SYNTAX itself is tested elsewhere.
		// We just check the counts below.
	}
	rows.Close()

	count3 := telemetry.GetRawFeatureCounts()["errorcodes."+pgcode.Syntax.String()]

	if count3-count2 != 1 {
		t.Fatalf("expected 1 syntax error, got %d", count3-count2)
	}
}

func TestTransactionRetryErrorCounts(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	telemetry.GetFeatureCounts(telemetry.Raw, telemetry.ResetCounts)

	// Transaction retry errors aren't given a pg error code until deep
	// in pgwire (pgwire.convertToErrWithPGCode). Make sure we're
	// reporting errors at a level that allows this code to be recorded.

	params, _ := createTestServerParamsAllowTenants()
	s, db, _ := serverutils.StartServer(t, params)
	defer s.Stopper().Stop(context.Background())

	if _, err := db.Exec("CREATE TABLE accounts (id INT8 PRIMARY KEY, balance INT8)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO accounts VALUES (1, 100)"); err != nil {
		t.Fatal(err)
	}

	txn1, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	txn2, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	for _, txn := range []*gosql.Tx{txn1, txn2} {
		rows, err := txn.Query("SELECT * FROM accounts WHERE id = 1")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
		}
		rows.Close()
	}

	for _, txn := range []*gosql.Tx{txn1, txn2} {
		if _, err := txn.Exec("UPDATE accounts SET balance = balance - 100 WHERE id = 1"); err != nil {
			if pqErr := (*pq.Error)(nil); errors.As(err, &pqErr) &&
				pgcode.MakeCode(string(pqErr.Code)) == pgcode.SerializationFailure {
				if err := txn.Rollback(); err != nil {
					t.Fatal(err)
				}
				return
			}
			t.Fatal(err)
		}
		if err := txn.Commit(); err != nil {
			if pqErr := (*pq.Error)(nil); !errors.As(err, &pqErr) ||
				pgcode.MakeCode(string(pqErr.Code)) != pgcode.SerializationFailure {
				t.Fatal(err)
			}
		}
	}

	if telemetry.GetRawFeatureCounts()["errorcodes.40001"] == 0 {
		t.Fatal("expected error code telemetry, got nothing")
	}
}
