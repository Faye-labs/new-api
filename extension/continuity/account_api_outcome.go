package continuity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	AccountAPIRequestIDHeader        = "X-Continuity-Account-Api-Request-Id"
	AccountAPIRequestTimestampHeader = "X-Continuity-Account-Api-Timestamp"
	AccountAPIRequestSignatureHeader = "X-Continuity-Account-Api-Signature"

	accountAPIRequestBindingVersion = "v1"
	accountAPIRequestMaxClockSkew   = 5 * time.Minute

	accountAPIRequestStateProcessing    = "processing"
	accountAPIRequestStateFinalized     = "finalized"
	accountAPIRequestStateIndeterminate = "indeterminate"
)

var (
	accountAPIRequestIDPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	accountAPIRequestSigPattern = regexp.MustCompile(`^v1=[0-9a-f]{64}$`)

	errAccountAPIRequestDuplicate       = errors.New("continuity Account API request already exists")
	errAccountAPIRequestOutcomeNotFound = errors.New("continuity Account API request outcome not found")
	errAccountAPIRequestLogChain        = errors.New("continuity Account API request log chain is indeterminate")
)

type continuityAccountAPIRequestOutcome struct {
	ID          uint   `gorm:"primaryKey"`
	UserID      int    `gorm:"uniqueIndex:idx_continuity_account_api_request,priority:1"`
	TokenID     int    `gorm:"uniqueIndex:idx_continuity_account_api_request,priority:2"`
	RequestID   string `gorm:"type:varchar(64);uniqueIndex:idx_continuity_account_api_request,priority:3"`
	State       string `gorm:"type:varchar(16);index"`
	HTTPStatus  int
	LogsJSON    string `gorm:"type:text"`
	StartedAt   int64  `gorm:"bigint"`
	FinalizedAt int64  `gorm:"bigint"`
}

func (continuityAccountAPIRequestOutcome) TableName() string {
	return "continuity_account_api_request_outcomes"
}

type continuityAccountAPIOutcomeLog struct {
	ID               int    `json:"id"`
	Type             int    `json:"type"`
	TokenID          int    `json:"token_id"`
	Username         string `json:"username"`
	TokenName        string `json:"token_name"`
	ModelName        string `json:"model_name"`
	Group            string `json:"group"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Quota            int    `json:"quota"`
	ChannelID        int    `json:"channel_id"`
	CreatedAt        int64  `json:"created_at"`
	Other            string `json:"other"`
	RequestID        string `json:"request_id"`
}

type continuityAccountAPIRequestOutcomeView struct {
	UserID      int                              `json:"user_id"`
	TokenID     int                              `json:"token_id"`
	RequestID   string                           `json:"request_id"`
	State       string                           `json:"state"`
	Finalized   bool                             `json:"finalized"`
	HTTPStatus  *int                             `json:"http_status"`
	Logs        []continuityAccountAPIOutcomeLog `json:"logs"`
	StartedAt   int64                            `json:"started_at"`
	FinalizedAt *int64                           `json:"finalized_at"`
}

type continuityAccountAPILogCollector struct {
	sync.Mutex
	userID    int
	tokenID   int
	requestID string
	closed    bool
	logs      []continuityAccountAPIOutcomeLog
}

func newContinuityAccountAPILogCollector(
	userID int,
	tokenID int,
	requestID string,
) *continuityAccountAPILogCollector {
	return &continuityAccountAPILogCollector{
		userID:    userID,
		tokenID:   tokenID,
		requestID: requestID,
		logs:      make([]continuityAccountAPIOutcomeLog, 0, 2),
	}
}

func (collector *continuityAccountAPILogCollector) collect(logRow model.Log) error {
	collector.Lock()
	defer collector.Unlock()
	if collector.closed ||
		logRow.UserId != collector.userID ||
		logRow.TokenId != collector.tokenID ||
		logRow.RequestId != collector.requestID {
		return errAccountAPIRequestLogChain
	}
	if logRow.Type == model.LogTypeConsume &&
		(logRow.Quota < 0 || logRow.PromptTokens < 0 || logRow.CompletionTokens < 0) {
		return errAccountAPIRequestLogChain
	}
	collector.logs = append(collector.logs, continuityAccountAPIOutcomeLog{
		ID:               logRow.Id,
		Type:             logRow.Type,
		TokenID:          logRow.TokenId,
		Username:         logRow.Username,
		TokenName:        logRow.TokenName,
		ModelName:        logRow.ModelName,
		Group:            logRow.Group,
		PromptTokens:     logRow.PromptTokens,
		CompletionTokens: logRow.CompletionTokens,
		Quota:            logRow.Quota,
		ChannelID:        logRow.ChannelId,
		CreatedAt:        logRow.CreatedAt,
		Other:            logRow.Other,
		RequestID:        logRow.RequestId,
	})
	return nil
}

func (collector *continuityAccountAPILogCollector) closeAndSnapshot() (
	[]continuityAccountAPIOutcomeLog,
	error,
) {
	collector.Lock()
	defer collector.Unlock()
	if collector.closed {
		return nil, errAccountAPIRequestLogChain
	}
	collector.closed = true
	logs := make([]continuityAccountAPIOutcomeLog, len(collector.logs))
	copy(logs, collector.logs)
	return logs, nil
}

var accountAPIOutcomeStorage = struct {
	sync.Mutex
	db    *gorm.DB
	ready bool
}{}

func ensureAccountAPIOutcomeStorage() error {
	if model.DB == nil {
		return errors.New("NewAPI main database is not initialized")
	}
	accountAPIOutcomeStorage.Lock()
	defer accountAPIOutcomeStorage.Unlock()
	if accountAPIOutcomeStorage.ready && accountAPIOutcomeStorage.db == model.DB {
		return nil
	}
	if err := model.DB.AutoMigrate(&continuityAccountAPIRequestOutcome{}); err != nil {
		return err
	}
	accountAPIOutcomeStorage.db = model.DB
	accountAPIOutcomeStorage.ready = true
	return nil
}

func normalizeAccountAPIRequestID(value string) (string, error) {
	if !accountAPIRequestIDPattern.MatchString(value) {
		return "", errInvalidRequest
	}
	return value, nil
}

func accountAPIRequestCanonicalPayload(
	timestamp string,
	requestID string,
	method string,
	path string,
	userID int,
	tokenID int,
) string {
	return strings.Join([]string{
		accountAPIRequestBindingVersion,
		timestamp,
		requestID,
		method,
		path,
		strconv.Itoa(userID),
		strconv.Itoa(tokenID),
	}, "\n")
}

func parseAccountAPIBearerToken(c *gin.Context) (*model.Token, error) {
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	parts := strings.Fields(authorization)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, errInvalidRequest
	}
	key := strings.TrimPrefix(parts[1], "sk-")
	keyParts := strings.Split(key, "-")
	if len(keyParts) == 0 || keyParts[0] == "" {
		return nil, errInvalidRequest
	}
	var token model.Token
	if model.DB == nil {
		return nil, errInvalidRequest
	}
	if err := model.DB.Where(&model.Token{Key: keyParts[0]}).First(&token).Error; err != nil {
		return nil, errInvalidRequest
	}
	if token.Status != common.TokenStatusEnabled ||
		(token.ExpiredTime != -1 && token.ExpiredTime < common.GetTimestamp()) ||
		(!token.UnlimitedQuota && token.RemainQuota <= 0) ||
		!model.IsContinuityAccountAPIToken(&token) {
		return nil, errInvalidRequest
	}
	return &token, nil
}

func verifyAccountAPIRequestBinding(c *gin.Context, token *model.Token) (string, error) {
	requestID, err := normalizeAccountAPIRequestID(c.GetHeader(AccountAPIRequestIDHeader))
	if err != nil {
		return "", err
	}
	timestampText := c.GetHeader(AccountAPIRequestTimestampHeader)
	timestamp, err := strconv.ParseInt(timestampText, 10, 64)
	if err != nil || strconv.FormatInt(timestamp, 10) != timestampText {
		return "", errInvalidRequest
	}
	requestTime := time.Unix(timestamp, 0)
	if requestTime.Before(time.Now().Add(-accountAPIRequestMaxClockSkew)) ||
		requestTime.After(time.Now().Add(accountAPIRequestMaxClockSkew)) {
		return "", errInvalidRequest
	}

	signature := c.GetHeader(AccountAPIRequestSignatureHeader)
	if !accountAPIRequestSigPattern.MatchString(signature) {
		return "", errInvalidRequest
	}
	secret := strings.TrimSpace(os.Getenv(ContinuityInternalAPISecretEnv))
	if !validContinuityInternalSecret(secret) {
		return "", errors.New("Continuity internal API is not configured")
	}
	payload := accountAPIRequestCanonicalPayload(
		timestampText,
		requestID,
		c.Request.Method,
		c.Request.URL.EscapedPath(),
		token.UserId,
		token.Id,
	)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	expected := mac.Sum(nil)
	provided, err := hex.DecodeString(strings.TrimPrefix(signature, accountAPIRequestBindingVersion+"="))
	if err != nil || !hmac.Equal(expected, provided) {
		return "", errInvalidRequest
	}
	return requestID, nil
}

func validContinuityInternalSecret(secret string) bool {
	if len(secret) < 32 || secret != strings.TrimSpace(secret) {
		return false
	}
	for _, character := range secret {
		if character <= ' ' || character == 0x7f {
			return false
		}
	}
	return true
}

func accountAPIRequestBindingHeadersPresent(c *gin.Context) bool {
	return c.GetHeader(AccountAPIRequestIDHeader) != "" ||
		c.GetHeader(AccountAPIRequestTimestampHeader) != "" ||
		c.GetHeader(AccountAPIRequestSignatureHeader) != ""
}

func clearAccountAPIRequestBindingHeaders(c *gin.Context) {
	c.Request.Header.Del(AccountAPIRequestIDHeader)
	c.Request.Header.Del(AccountAPIRequestTimestampHeader)
	c.Request.Header.Del(AccountAPIRequestSignatureHeader)
}

func beginAccountAPIRequestOutcome(token *model.Token, requestID string) (uint, error) {
	if err := ensureAccountAPIOutcomeStorage(); err != nil {
		return 0, err
	}
	outcome := continuityAccountAPIRequestOutcome{
		UserID:    token.UserId,
		TokenID:   token.Id,
		RequestID: requestID,
		State:     accountAPIRequestStateProcessing,
		LogsJSON:  "[]",
		StartedAt: time.Now().Unix(),
	}
	if err := model.DB.Create(&outcome).Error; err != nil {
		var existing continuityAccountAPIRequestOutcome
		lookupErr := model.DB.Where(
			"user_id = ? AND token_id = ? AND request_id = ?",
			token.UserId,
			token.Id,
			requestID,
		).First(&existing).Error
		if lookupErr == nil {
			return 0, errAccountAPIRequestDuplicate
		}
		return 0, err
	}
	return outcome.ID, nil
}

func markAccountAPIRequestIndeterminate(outcomeID uint) {
	if model.DB == nil || outcomeID == 0 {
		return
	}
	result := model.DB.Model(&continuityAccountAPIRequestOutcome{}).
		Where("id = ? AND state = ?", outcomeID, accountAPIRequestStateProcessing).
		Updates(map[string]interface{}{
			"state":        accountAPIRequestStateIndeterminate,
			"finalized_at": time.Now().Unix(),
		})
	if result.Error != nil {
		common.SysLog(fmt.Sprintf(
			"failed to mark Continuity Account API request indeterminate: %v",
			result.Error,
		))
	}
}

func finalizeAccountAPIRequestOutcome(
	c *gin.Context,
	outcomeID uint,
	collector *continuityAccountAPILogCollector,
) error {
	if !common.LogConsumeEnabled ||
		c.GetBool(common.ContinuityAccountAPILogWriteFailedKey) {
		markAccountAPIRequestIndeterminate(outcomeID)
		return errAccountAPIRequestLogChain
	}
	logs, err := collector.closeAndSnapshot()
	if err != nil {
		markAccountAPIRequestIndeterminate(outcomeID)
		return err
	}
	logsJSON, err := common.Marshal(logs)
	if err != nil {
		markAccountAPIRequestIndeterminate(outcomeID)
		return err
	}
	finalizedAt := time.Now().Unix()
	result := model.DB.Model(&continuityAccountAPIRequestOutcome{}).
		Where("id = ? AND state = ?", outcomeID, accountAPIRequestStateProcessing).
		Updates(map[string]interface{}{
			"state":        accountAPIRequestStateFinalized,
			"http_status":  c.Writer.Status(),
			"logs_json":    string(logsJSON),
			"finalized_at": finalizedAt,
		})
	if result.Error != nil {
		markAccountAPIRequestIndeterminate(outcomeID)
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errAccountAPIRequestLogChain
	}
	return nil
}

func getAccountAPIRequestOutcome(
	userID int,
	tokenID int,
	requestID string,
) (continuityAccountAPIRequestOutcomeView, error) {
	if userID <= 0 || tokenID <= 0 {
		return continuityAccountAPIRequestOutcomeView{}, errInvalidRequest
	}
	exactRequestID, err := normalizeAccountAPIRequestID(requestID)
	if err != nil {
		return continuityAccountAPIRequestOutcomeView{}, err
	}
	if err := ensureAccountAPIOutcomeStorage(); err != nil {
		return continuityAccountAPIRequestOutcomeView{}, err
	}
	var outcome continuityAccountAPIRequestOutcome
	if err := model.DB.Where(
		"user_id = ? AND token_id = ? AND request_id = ?",
		userID,
		tokenID,
		exactRequestID,
	).First(&outcome).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return continuityAccountAPIRequestOutcomeView{}, errAccountAPIRequestOutcomeNotFound
		}
		return continuityAccountAPIRequestOutcomeView{}, err
	}
	logs := make([]continuityAccountAPIOutcomeLog, 0)
	if outcome.State == accountAPIRequestStateFinalized {
		if err := common.Unmarshal([]byte(outcome.LogsJSON), &logs); err != nil {
			return continuityAccountAPIRequestOutcomeView{}, err
		}
	}
	view := continuityAccountAPIRequestOutcomeView{
		UserID:    outcome.UserID,
		TokenID:   outcome.TokenID,
		RequestID: outcome.RequestID,
		State:     outcome.State,
		Finalized: outcome.State == accountAPIRequestStateFinalized,
		Logs:      logs,
		StartedAt: outcome.StartedAt,
	}
	if outcome.State == accountAPIRequestStateFinalized {
		view.HTTPStatus = &outcome.HTTPStatus
		view.FinalizedAt = &outcome.FinalizedAt
	} else if outcome.State == accountAPIRequestStateIndeterminate {
		view.FinalizedAt = &outcome.FinalizedAt
	}
	return view, nil
}

func abortAccountAPIRequest(c *gin.Context, status int, code string, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"type":    "continuity_account_api_error",
			"code":    code,
			"message": message,
		},
	})
}

func accountAPITokenAuthGuard(c *gin.Context, token *model.Token) bool {
	if !model.IsContinuityAccountAPIToken(token) {
		return true
	}
	if c.GetBool(common.ContinuityAccountAPIRequestBoundKey) {
		return true
	}
	abortAccountAPIRequest(
		c,
		http.StatusUnauthorized,
		"trusted_request_binding_required",
		"Account API token requires a trusted request binding",
	)
	return false
}

// AccountAPIRequestFinality is inert for ordinary requests. A request that
// opts into the private binding protocol must authenticate an exact active
// Continuity-managed Account API token and present a path-bound HMAC. Only
// then is the public request id replaced. The outcome row is inserted before
// downstream work and becomes finalized only after the complete synchronous
// relay/log chain has returned and its exact log snapshot is durable.
func AccountAPIRequestFinality() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !accountAPIRequestBindingHeadersPresent(c) {
			c.Next()
			return
		}
		token, err := parseAccountAPIBearerToken(c)
		if err != nil {
			clearAccountAPIRequestBindingHeaders(c)
			abortAccountAPIRequest(
				c,
				http.StatusForbidden,
				"invalid_request_binding",
				"Invalid Account API request binding",
			)
			return
		}
		requestID, err := verifyAccountAPIRequestBinding(c, token)
		clearAccountAPIRequestBindingHeaders(c)
		if err != nil {
			status := http.StatusForbidden
			if !validContinuityInternalSecret(strings.TrimSpace(os.Getenv(ContinuityInternalAPISecretEnv))) {
				status = http.StatusServiceUnavailable
			}
			abortAccountAPIRequest(
				c,
				status,
				"invalid_request_binding",
				"Invalid Account API request binding",
			)
			return
		}
		if !common.LogConsumeEnabled {
			abortAccountAPIRequest(
				c,
				http.StatusServiceUnavailable,
				"usage_log_unavailable",
				"Account API usage logging is unavailable",
			)
			return
		}
		outcomeID, err := beginAccountAPIRequestOutcome(token, requestID)
		if err != nil {
			status := http.StatusServiceUnavailable
			code := "request_outcome_unavailable"
			message := "Account API request outcome storage is unavailable"
			if errors.Is(err, errAccountAPIRequestDuplicate) {
				status = http.StatusConflict
				code = "duplicate_request_id"
				message = "Account API request id has already been used"
			}
			abortAccountAPIRequest(c, status, code, message)
			return
		}

		collector := newContinuityAccountAPILogCollector(token.UserId, token.Id, requestID)
		c.Set(common.ContinuityAccountAPIRequestBoundKey, true)
		c.Set(
			common.ContinuityAccountAPILogCollectorKey,
			func(logRow model.Log) error { return collector.collect(logRow) },
		)
		c.Set(common.RequestIdKey, requestID)
		c.Request = c.Request.WithContext(
			context.WithValue(c.Request.Context(), common.RequestIdKey, requestID),
		)
		c.Header(common.RequestIdKey, requestID)
		c.Next()

		if err := finalizeAccountAPIRequestOutcome(
			c,
			outcomeID,
			collector,
		); err != nil {
			common.SysLog(fmt.Sprintf(
				"failed to finalize Continuity Account API request %s: %v",
				requestID,
				err,
			))
		}
	}
}
