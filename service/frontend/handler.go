// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package frontend

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/pborman/uuid"
	"github.com/uber/cadence/.gen/go/cadence/workflowserviceserver"
	"github.com/uber/cadence/.gen/go/health"
	"github.com/uber/cadence/.gen/go/health/metaserver"
	h "github.com/uber/cadence/.gen/go/history"
	m "github.com/uber/cadence/.gen/go/matching"
	gen "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/client/history"
	"github.com/uber/cadence/client/matching"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/service"

	"github.com/uber-common/bark"
	"github.com/uber-go/tally"
)

var _ workflowserviceserver.Interface = (*WorkflowHandler)(nil)

type (
	// WorkflowHandler - Thrift handler inteface for workflow service
	WorkflowHandler struct {
		domainCache        cache.DomainCache
		metadataMgr        persistence.MetadataManager
		historyMgr         persistence.HistoryManager
		visibitiltyMgr     persistence.VisibilityManager
		history            history.Client
		matching           matching.Client
		tokenSerializer    common.TaskTokenSerializer
		hSerializerFactory persistence.HistorySerializerFactory
		metricsClient      metrics.Client
		startWG            sync.WaitGroup
		rateLimiter        common.TokenBucket
		config             *Config
		service.Service
	}

	getHistoryContinuationToken struct {
		RunID            string
		NextEventID      int64
		PersistenceToken []byte
	}
)

var (
	errDomainNotSet               = &gen.BadRequestError{Message: "Domain not set on request."}
	errTaskTokenNotSet            = &gen.BadRequestError{Message: "Task token not set on request."}
	errTaskListNotSet             = &gen.BadRequestError{Message: "TaskList is not set on request."}
	errExecutionNotSet            = &gen.BadRequestError{Message: "Execution is not set on request."}
	errWorkflowIDNotSet           = &gen.BadRequestError{Message: "WorkflowId is not set on request."}
	errRunIDNotSet                = &gen.BadRequestError{Message: "RunId is not set on request."}
	errInvalidRunID               = &gen.BadRequestError{Message: "Invalid RunId."}
	errInvalidNextPageToken       = &gen.BadRequestError{Message: "Invalid NextPageToken."}
	errNextPageTokenRunIDMismatch = &gen.BadRequestError{Message: "RunID in the request does not match the NextPageToken."}
)

// NewWorkflowHandler creates a thrift handler for the cadence service
func NewWorkflowHandler(
	sVice service.Service, config *Config, metadataMgr persistence.MetadataManager,
	historyMgr persistence.HistoryManager, visibilityMgr persistence.VisibilityManager) *WorkflowHandler {
	handler := &WorkflowHandler{
		Service:            sVice,
		config:             config,
		metadataMgr:        metadataMgr,
		historyMgr:         historyMgr,
		visibitiltyMgr:     visibilityMgr,
		tokenSerializer:    common.NewJSONTaskTokenSerializer(),
		hSerializerFactory: persistence.NewHistorySerializerFactory(),
		domainCache:        cache.NewDomainCache(metadataMgr, sVice.GetLogger()),
		rateLimiter:        common.NewTokenBucket(config.RPS, common.NewRealTimeSource()),
	}
	// prevent us from trying to serve requests before handler's Start() is complete
	handler.startWG.Add(1)
	return handler
}

// Start starts the handler
func (wh *WorkflowHandler) Start() error {
	wh.Service.GetDispatcher().Register(workflowserviceserver.New(wh))
	wh.Service.GetDispatcher().Register(metaserver.New(wh))
	wh.Service.Start()
	var err error
	wh.history, err = wh.Service.GetClientFactory().NewHistoryClient()
	if err != nil {
		return err
	}
	wh.matching, err = wh.Service.GetClientFactory().NewMatchingClient()
	if err != nil {
		return err
	}
	wh.metricsClient = wh.Service.GetMetricsClient()
	wh.startWG.Done()
	return nil
}

// Stop stops the handler
func (wh *WorkflowHandler) Stop() {
	wh.metadataMgr.Close()
	wh.visibitiltyMgr.Close()
	wh.historyMgr.Close()
	wh.Service.Stop()
}

// Health is for health check
func (wh *WorkflowHandler) Health(ctx context.Context) (*health.HealthStatus, error) {
	wh.startWG.Wait()
	wh.GetLogger().Debug("Frontend health check endpoint reached.")
	hs := &health.HealthStatus{Ok: true, Msg: common.StringPtr("frontend good")}
	return hs, nil
}

// RegisterDomain creates a new domain which can be used as a container for all resources.  Domain is a top level
// entity within Cadence, used as a container for all resources like workflow executions, tasklists, etc.  Domain
// acts as a sandbox and provides isolation for all resources within the domain.  All resources belongs to exactly one
// domain.
func (wh *WorkflowHandler) RegisterDomain(ctx context.Context, registerRequest *gen.RegisterDomainRequest) error {

	scope := metrics.FrontendRegisterDomainScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if registerRequest.Name == nil || *registerRequest.Name == "" {
		return wh.error(errDomainNotSet, scope)
	}

	response, err := wh.metadataMgr.CreateDomain(&persistence.CreateDomainRequest{
		Name:        *registerRequest.Name,
		Status:      persistence.DomainStatusRegistered,
		OwnerEmail:  common.StringDefault(registerRequest.OwnerEmail),
		Description: common.StringDefault(registerRequest.Description),
		Retention:   common.Int32Default(registerRequest.WorkflowExecutionRetentionPeriodInDays),
		EmitMetric:  common.BoolDefault(registerRequest.EmitMetric),
	})

	if err != nil {
		return wh.error(err, scope)
	}

	// TODO: Log through logging framework.  We need to have good auditing of domain CRUD
	wh.GetLogger().Debugf("Register domain succeeded for name: %v, Id: %v", *registerRequest.Name, response.ID)
	return nil
}

// DescribeDomain returns the information and configuration for a registered domain.
func (wh *WorkflowHandler) DescribeDomain(ctx context.Context,
	describeRequest *gen.DescribeDomainRequest) (*gen.DescribeDomainResponse, error) {

	scope := metrics.FrontendDescribeDomainScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if describeRequest.Name == nil {
		return nil, wh.error(errDomainNotSet, scope)
	}

	resp, err := wh.metadataMgr.GetDomain(&persistence.GetDomainRequest{
		Name: *describeRequest.Name,
	})

	if err != nil {
		return nil, wh.error(err, scope)
	}

	response := &gen.DescribeDomainResponse{}
	response.DomainInfo, response.Configuration = createDomainResponse(resp.Info, resp.Config)

	return response, nil
}

// UpdateDomain is used to update the information and configuration for a registered domain.
func (wh *WorkflowHandler) UpdateDomain(ctx context.Context,
	updateRequest *gen.UpdateDomainRequest) (*gen.UpdateDomainResponse, error) {

	scope := metrics.FrontendUpdateDomainScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if updateRequest.Name == nil {
		return nil, wh.error(errDomainNotSet, scope)
	}

	getResponse, err0 := wh.metadataMgr.GetDomain(&persistence.GetDomainRequest{
		Name: *updateRequest.Name,
	})

	if err0 != nil {
		return nil, wh.error(err0, scope)
	}

	info := getResponse.Info
	config := getResponse.Config

	if updateRequest.UpdatedInfo != nil {
		updatedInfo := updateRequest.UpdatedInfo
		if updatedInfo.Description != nil {
			info.Description = *updatedInfo.Description
		}
		if updatedInfo.OwnerEmail != nil {
			info.OwnerEmail = *updatedInfo.OwnerEmail
		}
	}

	if updateRequest.Configuration != nil {
		updatedConfig := updateRequest.Configuration
		if updatedConfig.EmitMetric != nil {
			config.EmitMetric = *updatedConfig.EmitMetric
		}
		if updatedConfig.WorkflowExecutionRetentionPeriodInDays != nil {
			config.Retention = *updatedConfig.WorkflowExecutionRetentionPeriodInDays
		}
	}

	err := wh.metadataMgr.UpdateDomain(&persistence.UpdateDomainRequest{
		Info:   info,
		Config: config,
	})
	if err != nil {
		return nil, wh.error(err, scope)
	}

	response := &gen.UpdateDomainResponse{}
	response.DomainInfo, response.Configuration = createDomainResponse(info, config)
	return response, nil
}

// DeprecateDomain us used to update status of a registered domain to DEPRECATED.  Once the domain is deprecated
// it cannot be used to start new workflow executions.  Existing workflow executions will continue to run on
// deprecated domains.
func (wh *WorkflowHandler) DeprecateDomain(ctx context.Context, deprecateRequest *gen.DeprecateDomainRequest) error {

	scope := metrics.FrontendDeprecateDomainScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if deprecateRequest.Name == nil {
		return wh.error(errDomainNotSet, scope)
	}

	getResponse, err0 := wh.metadataMgr.GetDomain(&persistence.GetDomainRequest{
		Name: *deprecateRequest.Name,
	})

	if err0 != nil {
		return wh.error(err0, scope)
	}

	info := getResponse.Info
	info.Status = persistence.DomainStatusDeprecated
	config := getResponse.Config

	err := wh.metadataMgr.UpdateDomain(&persistence.UpdateDomainRequest{
		Info:   info,
		Config: config,
	})
	if err != nil {
		return wh.error(errDomainNotSet, scope)
	}
	return nil
}

// PollForActivityTask - Poll for an activity task.
func (wh *WorkflowHandler) PollForActivityTask(
	ctx context.Context,
	pollRequest *gen.PollForActivityTaskRequest) (*gen.PollForActivityTaskResponse, error) {

	scope := metrics.FrontendPollForActivityTaskScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if ok, _ := wh.rateLimiter.TryConsume(1); !ok {
		return nil, wh.error(createServiceBusyError(), scope)
	}

	wh.Service.GetLogger().Debug("Received PollForActivityTask")
	if pollRequest.Domain == nil {
		return nil, wh.error(errDomainNotSet, scope)
	}

	if err := wh.validateTaskList(pollRequest.TaskList, scope); err != nil {
		return nil, err
	}

	info, _, err := wh.domainCache.GetDomain(*pollRequest.Domain)
	if err != nil {
		return nil, wh.error(err, scope)
	}

	resp, err := wh.matching.PollForActivityTask(ctx, &m.PollForActivityTaskRequest{
		DomainUUID:  common.StringPtr(info.ID),
		PollRequest: pollRequest,
	})
	if err != nil {
		wh.Service.GetLogger().Errorf(
			"PollForActivityTask failed. TaskList: %v, Error: %v", *pollRequest.TaskList.Name, err)
		return nil, wh.error(err, scope)
	}
	return resp, nil
}

// PollForDecisionTask - Poll for a decision task.
func (wh *WorkflowHandler) PollForDecisionTask(
	ctx context.Context,
	pollRequest *gen.PollForDecisionTaskRequest) (*gen.PollForDecisionTaskResponse, error) {

	scope := metrics.FrontendPollForDecisionTaskScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if ok, _ := wh.rateLimiter.TryConsume(1); !ok {
		return nil, wh.error(createServiceBusyError(), scope)
	}

	wh.Service.GetLogger().Debug("Received PollForDecisionTask")
	if pollRequest.Domain == nil {
		return nil, wh.error(errDomainNotSet, scope)
	}

	if err := wh.validateTaskList(pollRequest.TaskList, scope); err != nil {
		return nil, err
	}

	domainName := *pollRequest.Domain
	info, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return nil, wh.error(err, scope)
	}

	wh.Service.GetLogger().Debugf("Poll for decision. DomainName: %v, DomainID: %v", domainName, info.ID)

	matchingResp, err := wh.matching.PollForDecisionTask(ctx, &m.PollForDecisionTaskRequest{
		DomainUUID:  common.StringPtr(info.ID),
		PollRequest: pollRequest,
	})
	if err != nil {
		wh.Service.GetLogger().Errorf(
			"PollForDecisionTask failed. TaskList: %v, Error: %v", *pollRequest.TaskList.Name, err)
		return nil, wh.error(err, scope)
	}

	var history *gen.History
	var persistenceToken []byte
	var continuation []byte
	if matchingResp.WorkflowExecution != nil {
		// Non-empty response. Get the history
		nextEventID := common.Int64Default(matchingResp.StartedEventId) + 1
		history, persistenceToken, err = wh.getHistory(
			info.ID,
			*matchingResp.WorkflowExecution,
			nextEventID,
			wh.config.DefaultHistoryMaxPageSize,
			nil)
		if err != nil {
			return nil, wh.error(err, scope)
		}

		if matchingResp.WorkflowExecution.RunId == nil {
			wh.Service.GetLogger().Errorf(
				"PollForDecisionTask from matching engine doesn't have run id. TaskList: %v",
				*pollRequest.TaskList.Name)
			return nil, wh.error(errRunIDNotSet, scope)
		}

		continuation, err =
			getSerializedGetHistoryToken(
				persistenceToken,
				*matchingResp.WorkflowExecution.RunId,
				history,
				nextEventID)
		if err != nil {
			return nil, wh.error(err, scope)
		}
	}

	return createPollForDecisionTaskResponse(matchingResp, history, continuation), nil
}

// RecordActivityTaskHeartbeat - Record Activity Task Heart beat.
func (wh *WorkflowHandler) RecordActivityTaskHeartbeat(
	ctx context.Context,
	heartbeatRequest *gen.RecordActivityTaskHeartbeatRequest) (*gen.RecordActivityTaskHeartbeatResponse, error) {

	scope := metrics.FrontendRecordActivityTaskHeartbeatScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	// Count the request in the RPS, but we still accept it even if RPS is exceeded
	wh.rateLimiter.TryConsume(1)

	wh.Service.GetLogger().Debug("Received RecordActivityTaskHeartbeat")
	if heartbeatRequest.TaskToken == nil {
		return nil, wh.error(errTaskTokenNotSet, scope)
	}
	taskToken, err := wh.tokenSerializer.Deserialize(heartbeatRequest.TaskToken)
	if err != nil {
		return nil, wh.error(err, scope)
	}
	if taskToken.DomainID == "" {
		return nil, wh.error(errDomainNotSet, scope)
	}

	resp, err := wh.history.RecordActivityTaskHeartbeat(ctx, &h.RecordActivityTaskHeartbeatRequest{
		DomainUUID:       common.StringPtr(taskToken.DomainID),
		HeartbeatRequest: heartbeatRequest,
	})
	if err != nil {
		return nil, wh.error(err, scope)
	}
	return resp, nil
}

// RespondActivityTaskCompleted - response to an activity task
func (wh *WorkflowHandler) RespondActivityTaskCompleted(
	ctx context.Context,
	completeRequest *gen.RespondActivityTaskCompletedRequest) error {

	scope := metrics.FrontendRespondActivityTaskCompletedScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	// Count the request in the RPS, but we still accept it even if RPS is exceeded
	wh.rateLimiter.TryConsume(1)

	if completeRequest.TaskToken == nil {
		return wh.error(errTaskTokenNotSet, scope)
	}
	taskToken, err := wh.tokenSerializer.Deserialize(completeRequest.TaskToken)
	if err != nil {
		return wh.error(err, scope)
	}
	if taskToken.DomainID == "" {
		return wh.error(errDomainNotSet, scope)
	}

	err = wh.history.RespondActivityTaskCompleted(ctx, &h.RespondActivityTaskCompletedRequest{
		DomainUUID:      common.StringPtr(taskToken.DomainID),
		CompleteRequest: completeRequest,
	})
	if err != nil {
		logger := wh.getLoggerForTask(completeRequest.TaskToken)
		logger.Errorf("RespondActivityTaskCompleted. Error: %v", err)
		return wh.error(err, scope)
	}
	return nil
}

// RespondActivityTaskFailed - response to an activity task failure
func (wh *WorkflowHandler) RespondActivityTaskFailed(
	ctx context.Context,
	failedRequest *gen.RespondActivityTaskFailedRequest) error {

	scope := metrics.FrontendRespondActivityTaskFailedScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	// Count the request in the RPS, but we still accept it even if RPS is exceeded
	wh.rateLimiter.TryConsume(1)

	if failedRequest.TaskToken == nil {
		return wh.error(errTaskTokenNotSet, scope)
	}
	taskToken, err := wh.tokenSerializer.Deserialize(failedRequest.TaskToken)
	if err != nil {
		return wh.error(err, scope)
	}
	if taskToken.DomainID == "" {
		return wh.error(errDomainNotSet, scope)
	}

	err = wh.history.RespondActivityTaskFailed(ctx, &h.RespondActivityTaskFailedRequest{
		DomainUUID:    common.StringPtr(taskToken.DomainID),
		FailedRequest: failedRequest,
	})
	if err != nil {
		logger := wh.getLoggerForTask(failedRequest.TaskToken)
		logger.Errorf("RespondActivityTaskFailed. Error: %v", err)
		return wh.error(err, scope)
	}
	return nil

}

// RespondActivityTaskCanceled - called to cancel an activity task
func (wh *WorkflowHandler) RespondActivityTaskCanceled(
	ctx context.Context,
	cancelRequest *gen.RespondActivityTaskCanceledRequest) error {

	scope := metrics.FrontendRespondActivityTaskCanceledScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	// Count the request in the RPS, but we still accept it even if RPS is exceeded
	wh.rateLimiter.TryConsume(1)

	if cancelRequest.TaskToken == nil {
		return wh.error(errTaskTokenNotSet, scope)
	}
	taskToken, err := wh.tokenSerializer.Deserialize(cancelRequest.TaskToken)
	if err != nil {
		return wh.error(err, scope)
	}
	if taskToken.DomainID == "" {
		return wh.error(errDomainNotSet, scope)
	}

	err = wh.history.RespondActivityTaskCanceled(ctx, &h.RespondActivityTaskCanceledRequest{
		DomainUUID:    common.StringPtr(taskToken.DomainID),
		CancelRequest: cancelRequest,
	})
	if err != nil {
		logger := wh.getLoggerForTask(cancelRequest.TaskToken)
		logger.Errorf("RespondActivityTaskCanceled. Error: %v", err)
		return wh.error(err, scope)
	}
	return nil

}

// RespondDecisionTaskCompleted - response to a decision task
func (wh *WorkflowHandler) RespondDecisionTaskCompleted(
	ctx context.Context,
	completeRequest *gen.RespondDecisionTaskCompletedRequest) error {

	scope := metrics.FrontendRespondDecisionTaskCompletedScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	// Count the request in the RPS, but we still accept it even if RPS is exceeded
	wh.rateLimiter.TryConsume(1)

	if completeRequest.TaskToken == nil {
		return wh.error(errTaskTokenNotSet, scope)
	}
	taskToken, err := wh.tokenSerializer.Deserialize(completeRequest.TaskToken)
	if err != nil {
		return wh.error(err, scope)
	}
	if taskToken.DomainID == "" {
		return wh.error(errDomainNotSet, scope)
	}

	err = wh.history.RespondDecisionTaskCompleted(ctx, &h.RespondDecisionTaskCompletedRequest{
		DomainUUID:      common.StringPtr(taskToken.DomainID),
		CompleteRequest: completeRequest,
	})
	if err != nil {
		logger := wh.getLoggerForTask(completeRequest.TaskToken)
		logger.Errorf("RespondDecisionTaskCompleted. Error: %v", err)
		return wh.error(err, scope)
	}
	return nil
}

// StartWorkflowExecution - Creates a new workflow execution
func (wh *WorkflowHandler) StartWorkflowExecution(
	ctx context.Context,
	startRequest *gen.StartWorkflowExecutionRequest) (*gen.StartWorkflowExecutionResponse, error) {

	scope := metrics.FrontendStartWorkflowExecutionScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if ok, _ := wh.rateLimiter.TryConsume(1); !ok {
		return nil, wh.error(createServiceBusyError(), scope)
	}

	if startRequest.Domain == nil {
		return nil, wh.error(errDomainNotSet, scope)
	}

	if startRequest.WorkflowId == nil || *startRequest.WorkflowId == "" {
		return nil, wh.error(&gen.BadRequestError{Message: "WorkflowId is not set on request."}, scope)
	}

	wh.Service.GetLogger().Debugf(
		"Received StartWorkflowExecution. WorkflowID: %v",
		*startRequest.WorkflowId)

	if startRequest.WorkflowType == nil ||
		startRequest.WorkflowType.Name == nil || *startRequest.WorkflowType.Name == "" {
		return nil, wh.error(&gen.BadRequestError{Message: "WorkflowType is not set on request."}, scope)
	}

	if err := wh.validateTaskList(startRequest.TaskList, scope); err != nil {
		return nil, err
	}

	if startRequest.ExecutionStartToCloseTimeoutSeconds == nil ||
		*startRequest.ExecutionStartToCloseTimeoutSeconds <= 0 {
		return nil, wh.error(&gen.BadRequestError{
			Message: "A valid ExecutionStartToCloseTimeoutSeconds is not set on request."}, scope)
	}

	if startRequest.TaskStartToCloseTimeoutSeconds == nil ||
		*startRequest.TaskStartToCloseTimeoutSeconds <= 0 {
		return nil, wh.error(&gen.BadRequestError{
			Message: "A valid TaskStartToCloseTimeoutSeconds is not set on request."}, scope)
	}

	domainName := *startRequest.Domain
	wh.Service.GetLogger().Debugf("Start workflow execution request domain: %v", domainName)
	info, _, err := wh.domainCache.GetDomain(domainName)
	if err != nil {
		return nil, wh.error(err, scope)
	}

	wh.Service.GetLogger().Debugf("Start workflow execution request domainID: %v", info.ID)

	resp, err := wh.history.StartWorkflowExecution(ctx, &h.StartWorkflowExecutionRequest{
		DomainUUID:   common.StringPtr(info.ID),
		StartRequest: startRequest,
	})
	if err != nil {
		wh.Service.GetLogger().Errorf("StartWorkflowExecution failed. WorkflowID: %v. Error: %v",
			*startRequest.WorkflowId, err)
		return nil, wh.error(err, scope)
	}
	return resp, nil
}

// GetWorkflowExecutionHistory - retrieves the hisotry of workflow execution
func (wh *WorkflowHandler) GetWorkflowExecutionHistory(
	ctx context.Context,
	getRequest *gen.GetWorkflowExecutionHistoryRequest) (*gen.GetWorkflowExecutionHistoryResponse, error) {

	scope := metrics.FrontendGetWorkflowExecutionHistoryScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if ok, _ := wh.rateLimiter.TryConsume(1); !ok {
		return nil, wh.error(createServiceBusyError(), scope)
	}

	if getRequest.Domain == nil {
		return nil, wh.error(errDomainNotSet, scope)
	}

	if err := wh.validateExecution(getRequest.Execution, scope); err != nil {
		return nil, err
	}

	if getRequest.MaximumPageSize == nil || *getRequest.MaximumPageSize == 0 {
		getRequest.MaximumPageSize = common.Int32Ptr(wh.config.DefaultHistoryMaxPageSize)
	}

	info, _, err := wh.domainCache.GetDomain(*getRequest.Domain)
	if err != nil {
		return nil, wh.error(err, scope)
	}

	token := &getHistoryContinuationToken{}
	if getRequest.NextPageToken != nil {
		token, err = deserializeGetHistoryToken(getRequest.NextPageToken)
		if err != nil {
			return nil, wh.error(errInvalidNextPageToken, scope)
		}
		if getRequest.Execution.RunId != nil && *getRequest.Execution.RunId != token.RunID {
			return nil, wh.error(errNextPageTokenRunIDMismatch, scope)
		}
	} else {
		response, err := wh.history.GetWorkflowExecutionNextEventID(ctx, &h.GetWorkflowExecutionNextEventIDRequest{
			DomainUUID: common.StringPtr(info.ID),
			Execution:  getRequest.Execution,
		})
		if err == nil {
			token.NextEventID = *response.EventId
			token.RunID = *response.RunId
		} else {
			if _, ok := err.(*gen.EntityNotExistsError); !ok || getRequest.Execution.RunId == nil {
				return nil, wh.error(err, scope)
			}
			// It is possible that we still have the events in the table even though the mutable state is gone
			// Get the nextEventID from visibility store if we still have it.
			visibilityResp, err := wh.visibitiltyMgr.GetClosedWorkflowExecution(&persistence.GetClosedWorkflowExecutionRequest{
				DomainUUID: info.ID,
				Execution:  *getRequest.Execution,
			})
			if err != nil {
				return nil, wh.error(err, scope)
			}
			token.NextEventID = *visibilityResp.Execution.HistoryLength
			token.RunID = *visibilityResp.Execution.Execution.RunId
		}
	}

	we := gen.WorkflowExecution{
		WorkflowId: getRequest.Execution.WorkflowId,
		RunId:      common.StringPtr(token.RunID),
	}
	history, persistenceToken, err :=
		wh.getHistory(info.ID, we, token.NextEventID, *getRequest.MaximumPageSize, token.PersistenceToken)
	if err != nil {
		return nil, wh.error(err, scope)
	}

	nextToken, err := getSerializedGetHistoryToken(persistenceToken, token.RunID, history, token.NextEventID)
	if err != nil {
		return nil, wh.error(err, scope)
	}

	return createGetWorkflowExecutionHistoryResponse(history, token.NextEventID, nextToken), nil
}

// SignalWorkflowExecution is used to send a signal event to running workflow execution.  This results in
// WorkflowExecutionSignaled event recorded in the history and a decision task being created for the execution.
func (wh *WorkflowHandler) SignalWorkflowExecution(ctx context.Context,
	signalRequest *gen.SignalWorkflowExecutionRequest) error {

	scope := metrics.FrontendSignalWorkflowExecutionScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if ok, _ := wh.rateLimiter.TryConsume(1); !ok {
		return wh.error(createServiceBusyError(), scope)
	}

	if signalRequest.Domain == nil {
		return wh.error(errDomainNotSet, scope)
	}

	if err := wh.validateExecution(signalRequest.WorkflowExecution, scope); err != nil {
		return err
	}

	if signalRequest.SignalName == nil {
		return wh.error(&gen.BadRequestError{Message: "SignalName is not set on request."}, scope)
	}

	info, _, err := wh.domainCache.GetDomain(*signalRequest.Domain)
	if err != nil {
		return wh.error(err, scope)
	}

	err = wh.history.SignalWorkflowExecution(ctx, &h.SignalWorkflowExecutionRequest{
		DomainUUID:    common.StringPtr(info.ID),
		SignalRequest: signalRequest,
	})
	if err != nil {
		return wh.error(err, scope)
	}

	return nil
}

// TerminateWorkflowExecution terminates an existing workflow execution by recording WorkflowExecutionTerminated event
// in the history and immediately terminating the execution instance.
func (wh *WorkflowHandler) TerminateWorkflowExecution(ctx context.Context,
	terminateRequest *gen.TerminateWorkflowExecutionRequest) error {

	scope := metrics.FrontendTerminateWorkflowExecutionScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if ok, _ := wh.rateLimiter.TryConsume(1); !ok {
		return wh.error(createServiceBusyError(), scope)
	}

	if terminateRequest.Domain == nil {
		return wh.error(errDomainNotSet, scope)
	}

	if err := wh.validateExecution(terminateRequest.WorkflowExecution, scope); err != nil {
		return err
	}

	info, _, err := wh.domainCache.GetDomain(*terminateRequest.Domain)
	if err != nil {
		return wh.error(err, scope)
	}

	err = wh.history.TerminateWorkflowExecution(ctx, &h.TerminateWorkflowExecutionRequest{
		DomainUUID:       common.StringPtr(info.ID),
		TerminateRequest: terminateRequest,
	})
	if err != nil {
		return wh.error(err, scope)
	}

	return nil
}

// RequestCancelWorkflowExecution - requests to cancel a workflow execution
func (wh *WorkflowHandler) RequestCancelWorkflowExecution(
	ctx context.Context,
	cancelRequest *gen.RequestCancelWorkflowExecutionRequest) error {

	scope := metrics.FrontendRequestCancelWorkflowExecutionScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if ok, _ := wh.rateLimiter.TryConsume(1); !ok {
		return wh.error(createServiceBusyError(), scope)
	}

	if cancelRequest.Domain == nil {
		return wh.error(errDomainNotSet, scope)
	}

	if err := wh.validateExecution(cancelRequest.WorkflowExecution, scope); err != nil {
		return err
	}

	info, _, err := wh.domainCache.GetDomain(*cancelRequest.Domain)
	if err != nil {
		return wh.error(err, scope)
	}

	err = wh.history.RequestCancelWorkflowExecution(ctx, &h.RequestCancelWorkflowExecutionRequest{
		DomainUUID:    common.StringPtr(info.ID),
		CancelRequest: cancelRequest,
	})
	if err != nil {
		return wh.error(err, scope)
	}

	return nil
}

// ListOpenWorkflowExecutions - retrieves info for open workflow executions in a domain
func (wh *WorkflowHandler) ListOpenWorkflowExecutions(ctx context.Context,
	listRequest *gen.ListOpenWorkflowExecutionsRequest) (*gen.ListOpenWorkflowExecutionsResponse, error) {

	scope := metrics.FrontendListOpenWorkflowExecutionsScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if ok, _ := wh.rateLimiter.TryConsume(1); !ok {
		return nil, wh.error(createServiceBusyError(), scope)
	}

	if listRequest.Domain == nil {
		return nil, wh.error(errDomainNotSet, scope)
	}

	if listRequest.StartTimeFilter == nil {
		return nil, wh.error(&gen.BadRequestError{Message: "StartTimeFilter is required"}, scope)
	}

	if listRequest.StartTimeFilter.EarliestTime == nil {
		return nil, wh.error(&gen.BadRequestError{Message: "EarliestTime in StartTimeFilter is required"}, scope)
	}

	if listRequest.StartTimeFilter.LatestTime == nil {
		return nil, wh.error(&gen.BadRequestError{Message: "LatestTime in StartTimeFilter is required"}, scope)
	}

	if listRequest.ExecutionFilter != nil && listRequest.TypeFilter != nil {
		return nil, wh.error(&gen.BadRequestError{
			Message: "Only one of ExecutionFilter or TypeFilter is allowed"}, scope)
	}

	if listRequest.MaximumPageSize == nil || *listRequest.MaximumPageSize == 0 {
		listRequest.MaximumPageSize = common.Int32Ptr(wh.config.DefaultVisibilityMaxPageSize)
	}

	domainInfo, _, err := wh.domainCache.GetDomain(*listRequest.Domain)
	if err != nil {
		return nil, wh.error(err, scope)
	}

	baseReq := persistence.ListWorkflowExecutionsRequest{
		DomainUUID:        domainInfo.ID,
		PageSize:          int(*listRequest.MaximumPageSize),
		NextPageToken:     listRequest.NextPageToken,
		EarliestStartTime: *listRequest.StartTimeFilter.EarliestTime,
		LatestStartTime:   *listRequest.StartTimeFilter.LatestTime,
	}

	var persistenceResp *persistence.ListWorkflowExecutionsResponse
	if listRequest.ExecutionFilter != nil {
		persistenceResp, err = wh.visibitiltyMgr.ListOpenWorkflowExecutionsByWorkflowID(
			&persistence.ListWorkflowExecutionsByWorkflowIDRequest{
				ListWorkflowExecutionsRequest: baseReq,
				WorkflowID:                    *listRequest.ExecutionFilter.WorkflowId,
			})
	} else if listRequest.TypeFilter != nil {
		persistenceResp, err = wh.visibitiltyMgr.ListOpenWorkflowExecutionsByType(&persistence.ListWorkflowExecutionsByTypeRequest{
			ListWorkflowExecutionsRequest: baseReq,
			WorkflowTypeName:              *listRequest.TypeFilter.Name,
		})
	} else {
		persistenceResp, err = wh.visibitiltyMgr.ListOpenWorkflowExecutions(&baseReq)
	}

	if err != nil {
		return nil, wh.error(err, scope)
	}

	resp := &gen.ListOpenWorkflowExecutionsResponse{}
	resp.Executions = persistenceResp.Executions
	resp.NextPageToken = persistenceResp.NextPageToken
	return resp, nil
}

// ListClosedWorkflowExecutions - retrieves info for closed workflow executions in a domain
func (wh *WorkflowHandler) ListClosedWorkflowExecutions(ctx context.Context,
	listRequest *gen.ListClosedWorkflowExecutionsRequest) (*gen.ListClosedWorkflowExecutionsResponse, error) {

	scope := metrics.FrontendListClosedWorkflowExecutionsScope
	sw := wh.startRequestProfile(scope)
	defer sw.Stop()

	if ok, _ := wh.rateLimiter.TryConsume(1); !ok {
		return nil, wh.error(createServiceBusyError(), scope)
	}

	if listRequest.Domain == nil {
		return nil, wh.error(errDomainNotSet, scope)
	}

	if listRequest.StartTimeFilter == nil {
		return nil, wh.error(&gen.BadRequestError{Message: "StartTimeFilter is required"}, scope)
	}

	if listRequest.StartTimeFilter.EarliestTime == nil {
		return nil, wh.error(&gen.BadRequestError{Message: "EarliestTime in StartTimeFilter is required"}, scope)
	}

	if listRequest.StartTimeFilter.LatestTime == nil {
		return nil, wh.error(&gen.BadRequestError{Message: "LatestTime in StartTimeFilter is required"}, scope)
	}

	filterCount := 0
	if listRequest.ExecutionFilter != nil {
		filterCount++
	}
	if listRequest.TypeFilter != nil {
		filterCount++
	}
	if listRequest.StatusFilter != nil {
		filterCount++
	}

	if filterCount > 1 {
		return nil, wh.error(&gen.BadRequestError{
			Message: "Only one of ExecutionFilter, TypeFilter or StatusFilter is allowed"}, scope)
	}

	if listRequest.MaximumPageSize == nil || *listRequest.MaximumPageSize == 0 {
		listRequest.MaximumPageSize = common.Int32Ptr(wh.config.DefaultVisibilityMaxPageSize)
	}

	domainInfo, _, err := wh.domainCache.GetDomain(*listRequest.Domain)
	if err != nil {
		return nil, wh.error(err, scope)
	}

	baseReq := persistence.ListWorkflowExecutionsRequest{
		DomainUUID:        domainInfo.ID,
		PageSize:          int(*listRequest.MaximumPageSize),
		NextPageToken:     listRequest.NextPageToken,
		EarliestStartTime: *listRequest.StartTimeFilter.EarliestTime,
		LatestStartTime:   *listRequest.StartTimeFilter.LatestTime,
	}

	var persistenceResp *persistence.ListWorkflowExecutionsResponse
	if listRequest.ExecutionFilter != nil {
		persistenceResp, err = wh.visibitiltyMgr.ListClosedWorkflowExecutionsByWorkflowID(
			&persistence.ListWorkflowExecutionsByWorkflowIDRequest{
				ListWorkflowExecutionsRequest: baseReq,
				WorkflowID:                    *listRequest.ExecutionFilter.WorkflowId,
			})
	} else if listRequest.TypeFilter != nil {
		persistenceResp, err = wh.visibitiltyMgr.ListClosedWorkflowExecutionsByType(&persistence.ListWorkflowExecutionsByTypeRequest{
			ListWorkflowExecutionsRequest: baseReq,
			WorkflowTypeName:              *listRequest.TypeFilter.Name,
		})
	} else if listRequest.StatusFilter != nil {
		persistenceResp, err = wh.visibitiltyMgr.ListClosedWorkflowExecutionsByStatus(&persistence.ListClosedWorkflowExecutionsByStatusRequest{
			ListWorkflowExecutionsRequest: baseReq,
			Status: *listRequest.StatusFilter,
		})
	} else {
		persistenceResp, err = wh.visibitiltyMgr.ListClosedWorkflowExecutions(&baseReq)
	}

	if err != nil {
		return nil, wh.error(err, scope)
	}

	resp := &gen.ListClosedWorkflowExecutionsResponse{}
	resp.Executions = persistenceResp.Executions
	resp.NextPageToken = persistenceResp.NextPageToken
	return resp, nil
}

func (wh *WorkflowHandler) getHistory(domainID string, execution gen.WorkflowExecution,
	nextEventID int64, pageSize int32, nextPageToken []byte) (*gen.History, []byte, error) {

	if nextPageToken == nil {
		nextPageToken = []byte{}
	}
	historyEvents := []*gen.HistoryEvent{}

	response, err := wh.historyMgr.GetWorkflowExecutionHistory(&persistence.GetWorkflowExecutionHistoryRequest{
		DomainID:      domainID,
		Execution:     execution,
		NextEventID:   nextEventID,
		PageSize:      int(pageSize),
		NextPageToken: nextPageToken,
	})

	if err != nil {
		return nil, nil, err
	}

	for _, e := range response.Events {
		setSerializedHistoryDefaults(&e)
		s, _ := wh.hSerializerFactory.Get(e.EncodingType)
		history, err1 := s.Deserialize(&e)
		if err1 != nil {
			return nil, nil, err1
		}
		historyEvents = append(historyEvents, history.Events...)
	}

	nextPageToken = response.NextPageToken

	executionHistory := &gen.History{}
	executionHistory.Events = historyEvents
	return executionHistory, nextPageToken, nil
}

// sets the version and encoding types to defaults if they
// are missing from persistence. This is purely for backwards
// compatibility
func setSerializedHistoryDefaults(history *persistence.SerializedHistoryEventBatch) {
	if history.Version == 0 {
		history.Version = persistence.GetDefaultHistoryVersion()
	}
	if len(history.EncodingType) == 0 {
		history.EncodingType = persistence.DefaultEncodingType
	}
}

func (wh *WorkflowHandler) getLoggerForTask(taskToken []byte) bark.Logger {
	logger := wh.Service.GetLogger()
	task, err := wh.tokenSerializer.Deserialize(taskToken)
	if err == nil {
		logger = logger.WithFields(bark.Fields{
			"WorkflowID": task.WorkflowID,
			"RunID":      task.RunID,
			"ScheduleID": task.ScheduleID,
		})
	}
	return logger
}

// startRequestProfile initiates recording of request metrics
func (wh *WorkflowHandler) startRequestProfile(scope int) tally.Stopwatch {
	wh.startWG.Wait()
	sw := wh.metricsClient.StartTimer(scope, metrics.CadenceLatency)
	wh.metricsClient.IncCounter(scope, metrics.CadenceRequests)
	return sw
}

func (wh *WorkflowHandler) error(err error, scope int) error {
	switch err.(type) {
	case *gen.InternalServiceError:
		wh.metricsClient.IncCounter(scope, metrics.CadenceFailures)
		return err
	case *gen.BadRequestError:
		wh.metricsClient.IncCounter(scope, metrics.CadenceErrBadRequestCounter)
		return err
	case *gen.ServiceBusyError:
		wh.metricsClient.IncCounter(scope, metrics.CadenceErrServiceBusyCounter)
		return err
	case *gen.EntityNotExistsError:
		wh.metricsClient.IncCounter(scope, metrics.CadenceErrEntityNotExistsCounter)
		return err
	case *gen.WorkflowExecutionAlreadyStartedError:
		wh.metricsClient.IncCounter(scope, metrics.CadenceErrExecutionAlreadyStartedCounter)
		return err
	case *gen.DomainAlreadyExistsError:
		wh.metricsClient.IncCounter(scope, metrics.CadenceErrDomainAlreadyExistsCounter)
		return err
	case *gen.CancellationAlreadyRequestedError:
		wh.metricsClient.IncCounter(scope, metrics.CadenceErrCancellationAlreadyRequestedCounter)
		return err
	default:
		wh.metricsClient.IncCounter(scope, metrics.CadenceFailures)
		return &gen.InternalServiceError{Message: err.Error()}
	}
}

func (wh *WorkflowHandler) validateTaskList(t *gen.TaskList, scope int) error {
	if t == nil || t.Name == nil || *t.Name == "" {
		return wh.error(errTaskListNotSet, scope)
	}
	return nil
}

func (wh *WorkflowHandler) validateExecution(w *gen.WorkflowExecution, scope int) error {
	if w == nil {
		return wh.error(errExecutionNotSet, scope)
	}
	if w.WorkflowId == nil || *w.WorkflowId == "" {
		return wh.error(errWorkflowIDNotSet, scope)
	}
	if w.RunId != nil && uuid.Parse(*w.RunId) == nil {
		return wh.error(errInvalidRunID, scope)
	}
	return nil
}

func getDomainStatus(info *persistence.DomainInfo) *gen.DomainStatus {
	switch info.Status {
	case persistence.DomainStatusRegistered:
		v := gen.DomainStatusRegistered
		return &v
	case persistence.DomainStatusDeprecated:
		v := gen.DomainStatusDeprecated
		return &v
	case persistence.DomainStatusDeleted:
		v := gen.DomainStatusDeleted
		return &v
	}

	return nil
}

func createDomainResponse(info *persistence.DomainInfo, config *persistence.DomainConfig) (*gen.DomainInfo,
	*gen.DomainConfiguration) {

	i := &gen.DomainInfo{}
	i.Name = common.StringPtr(info.Name)
	i.Status = getDomainStatus(info)
	i.Description = common.StringPtr(info.Description)
	i.OwnerEmail = common.StringPtr(info.OwnerEmail)

	c := &gen.DomainConfiguration{}
	c.EmitMetric = common.BoolPtr(config.EmitMetric)
	c.WorkflowExecutionRetentionPeriodInDays = common.Int32Ptr(config.Retention)

	return i, c
}

func createPollForDecisionTaskResponse(
	matchingResponse *m.PollForDecisionTaskResponse, history *gen.History, nextPageToken []byte) *gen.PollForDecisionTaskResponse {
	resp := &gen.PollForDecisionTaskResponse{}
	if matchingResponse != nil {
		resp.TaskToken = matchingResponse.TaskToken
		resp.WorkflowExecution = matchingResponse.WorkflowExecution
		resp.WorkflowType = matchingResponse.WorkflowType
		resp.PreviousStartedEventId = matchingResponse.PreviousStartedEventId
		resp.StartedEventId = matchingResponse.StartedEventId
	}
	resp.History = history
	resp.NextPageToken = nextPageToken
	return resp
}

func createGetWorkflowExecutionHistoryResponse(
	history *gen.History, nextEventID int64, nextPageToken []byte) *gen.GetWorkflowExecutionHistoryResponse {
	resp := &gen.GetWorkflowExecutionHistoryResponse{}
	resp.History = history
	resp.NextPageToken = nextPageToken
	return resp
}

func deserializeGetHistoryToken(data []byte) (*getHistoryContinuationToken, error) {
	var token getHistoryContinuationToken
	err := json.Unmarshal(data, &token)

	return &token, err
}

func getSerializedGetHistoryToken(persistenceToken []byte, runID string, history *gen.History, nextEventID int64) ([]byte, error) {
	// create token if there are more events to read
	if history == nil {
		return nil, nil
	}
	events := history.Events
	if len(persistenceToken) > 0 && len(events) > 0 && *events[len(events)-1].EventId < nextEventID-1 {
		token := &getHistoryContinuationToken{
			RunID:            runID,
			NextEventID:      nextEventID,
			PersistenceToken: persistenceToken,
		}
		data, err := json.Marshal(token)

		return data, err
	}
	return nil, nil
}

func createServiceBusyError() *gen.ServiceBusyError {
	err := &gen.ServiceBusyError{}
	err.Message = "Too many outstanding requests to the cadence service"
	return err
}
