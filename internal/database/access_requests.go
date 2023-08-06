package database

import (
	"context"
	"fmt"

	"github.com/keegancsmith/sqlf"
	"github.com/sourcegraph/log"

	"github.com/sourcegraph/sourcegraph/internal/database/basestore"
	"github.com/sourcegraph/sourcegraph/internal/database/dbutil"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

const (
	errorCodeUserWithEmailExists          = "err_user_with_such_email_exists"
	errorCodeAccessRequestWithEmailExists = "err_access_request_with_such_email_exists"
)

// ErrCannotCreateAccessRequest is the error that is returned when a request_access cannot be added to the DB due to a constraint.
type ErrCannotCreateAccessRequest struct {
	code string
}

func (err ErrCannotCreateAccessRequest) Error() string {
	return fmt.Sprintf("cannot create user: %v", err.code)
}

// ErrAccessRequestNotFound is the error that is returned when a request_access cannot be found in the DB.
type ErrAccessRequestNotFound struct {
	ID    int32
	Email string
}

func (e *ErrAccessRequestNotFound) Error() string {
	if e.Email != "" {
		return fmt.Sprintf("access_request with email %q not found", e.Email)
	}

	return fmt.Sprintf("access_request with ID %d not found", e.ID)
}

func (e *ErrAccessRequestNotFound) NotFound() bool {
	return true
}

// IsAccessRequestUserWithEmailExists reports whether err is an error indicating that the access request email was already taken by a signed in user.
func IsAccessRequestUserWithEmailExists(err error) bool {
	var e ErrCannotCreateAccessRequest
	return errors.As(err, &e) && e.code == errorCodeUserWithEmailExists
}

// IsAccessRequestWithEmailExists reports whether err is an error indicating that the access request was already created.
func IsAccessRequestWithEmailExists(err error) bool {
	var e ErrCannotCreateAccessRequest
	return errors.As(err, &e) && e.code == errorCodeAccessRequestWithEmailExists
}

type AccessRequestsFilterArgs struct {
	Status *types.AccessRequestStatus
}

func (o *AccessRequestsFilterArgs) SQL() []*sqlf.Query {
	conds := []*sqlf.Query{sqlf.Sprintf("TRUE")}
	if o != nil && o.Status != nil {
		conds = append(conds, sqlf.Sprintf("status = %v", *o.Status))
	}
	return conds
}

// AccessRequestStore provides access to the `access_requests` table.
//
// For a detailed overview of the schema, see schema.md.
type AccessRequestStore interface {
	basestore.ShareableStore
	Count(context.Context, *AccessRequestsFilterArgs) (int, error)
	List(context.Context, *AccessRequestsFilterArgs, *PaginationArgs) (_ []*types.AccessRequest, err error)
	WithTransact(context.Context, func(AccessRequestStore) error) error
	Done(error) error
}

type accessRequestStore struct {
	*basestore.Store
	logger log.Logger
}

// AccessRequestsWith instantiates and returns a new accessRequestStore using the other store handle.
func AccessRequestsWith(other basestore.ShareableStore, logger log.Logger) AccessRequestStore {
	return &accessRequestStore{Store: basestore.NewWithHandle(other.Handle()), logger: logger}
}

const (
	accessRequestInsertQuery = `
		INSERT INTO access_requests (%s)
		VALUES ( %s, %s, %s, %s )
		RETURNING %s`
	accessRequestListQuery = `
		SELECT %s
		FROM access_requests
		WHERE (%s)`
	accessRequestUpdateQuery = `
		UPDATE access_requests
		SET status = %s, updated_at = NOW(), decision_by_user_id = %s
		WHERE id = %s
		RETURNING %s`
)

type AccessRequestListColumn string

const (
	AccessRequestListID AccessRequestListColumn = "id"
)

var (
	accessRequestColumns = []*sqlf.Query{
		sqlf.Sprintf("id"),
		sqlf.Sprintf("created_at"),
		sqlf.Sprintf("updated_at"),
		sqlf.Sprintf("name"),
		sqlf.Sprintf("email"),
		sqlf.Sprintf("status"),
		sqlf.Sprintf("additional_info"),
		sqlf.Sprintf("decision_by_user_id"),
	}
	accessRequestInsertColumns = []*sqlf.Query{
		sqlf.Sprintf("name"),
		sqlf.Sprintf("email"),
		sqlf.Sprintf("additional_info"),
		sqlf.Sprintf("status"),
	}
)

func (s *accessRequestStore) Count(ctx context.Context, fArgs *AccessRequestsFilterArgs) (int, error) {
	q := sqlf.Sprintf("SELECT COUNT(*) FROM access_requests WHERE (%s)", sqlf.Join(fArgs.SQL(), ") AND ("))
	return basestore.ScanInt(s.QueryRow(ctx, q))
}

func (s *accessRequestStore) List(ctx context.Context, fArgs *AccessRequestsFilterArgs, pArgs *PaginationArgs) ([]*types.AccessRequest, error) {
	if fArgs == nil {
		fArgs = &AccessRequestsFilterArgs{}
	}
	where := fArgs.SQL()
	if pArgs == nil {
		pArgs = &PaginationArgs{}
	}
	p := pArgs.SQL()

	if p.Where != nil {
		where = append(where, p.Where)
	}

	q := sqlf.Sprintf(accessRequestListQuery, sqlf.Join(accessRequestColumns, ","), sqlf.Join(where, ") AND ("))
	q = p.AppendOrderToQuery(q)
	q = p.AppendLimitToQuery(q)

	nodes, err := scanAccessRequests(s.Query(ctx, q))
	if err != nil {
		return nil, err
	}

	return nodes, nil
}

func (s *accessRequestStore) WithTransact(ctx context.Context, f func(tx AccessRequestStore) error) error {
	return s.Store.WithTransact(ctx, func(tx *basestore.Store) error {
		return f(&accessRequestStore{
			logger: s.logger,
			Store:  tx,
		})
	})
}

func scanAccessRequest(sc dbutil.Scanner) (*types.AccessRequest, error) {
	var accessRequest types.AccessRequest
	if err := sc.Scan(&accessRequest.ID, &accessRequest.CreatedAt, &accessRequest.UpdatedAt, &accessRequest.Name, &accessRequest.Email, &accessRequest.Status, &accessRequest.AdditionalInfo, &accessRequest.DecisionByUserID); err != nil {
		return nil, err
	}

	return &accessRequest, nil
}

var scanAccessRequests = basestore.NewSliceScanner(scanAccessRequest)
