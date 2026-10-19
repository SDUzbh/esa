package esa

// Typed states. Each entity has its own type, so a state of one cannot be
// assigned to another without a visible conversion, and a misspelled
// constant does not compile. The JSON and database encodings are the
// unchanged strings.

// InvestigationState is the state of an investigation (a shared task).
type InvestigationState string

const (
	InvestigationQueued                InvestigationState = "queued"
	InvestigationRunning               InvestigationState = "running"
	InvestigationInvestigating         InvestigationState = "investigating"
	InvestigationAwaitingConfiguration InvestigationState = "awaiting_configuration"
	InvestigationAwaitingForward       InvestigationState = "awaiting_forward"
	InvestigationAwaitingInformation   InvestigationState = "awaiting_information"
	InvestigationAgentProposed         InvestigationState = "agent_proposed"
	InvestigationReuseProposed         InvestigationState = "reuse_proposed"
	InvestigationApplied               InvestigationState = "applied"
	InvestigationJoined                InvestigationState = "joined"
	InvestigationReturnedToSupport     InvestigationState = "returned_to_support"
	InvestigationTrusted               InvestigationState = "trusted"
	InvestigationRejected              InvestigationState = "rejected"
	InvestigationWithdrawn             InvestigationState = "withdrawn"
	InvestigationFailed                InvestigationState = "failed"
	InvestigationSuperseded            InvestigationState = "superseded"
)

// RunState is the state of a run, projected from its investigation;
// issue_removed, membership_reviewed and restricted are also shown on a
// ticket's current result.
type RunState string

const (
	RunQueued                RunState = "queued"
	RunRunning               RunState = "running"
	RunInvestigating         RunState = "investigating"
	RunAwaitingConfiguration RunState = "awaiting_configuration"
	RunAwaitingForward       RunState = "awaiting_forward"
	RunAwaitingInformation   RunState = "awaiting_information"
	RunAwaitingReview        RunState = "awaiting_review"
	RunCompleted             RunState = "completed"
	RunJoined                RunState = "joined"
	RunReturnedToSupport     RunState = "returned_to_support"
	RunRejected              RunState = "rejected"
	RunInvalidated           RunState = "invalidated"
	RunRestricted            RunState = "restricted"
	RunFailed                RunState = "failed"
	RunSuperseded            RunState = "superseded"
	RunIssueRemoved          RunState = "issue_removed"
	RunMembershipReviewed    RunState = "membership_reviewed"
)

// CaseState is the state of a collected case.
type CaseState string

const (
	CaseDraft       CaseState = "draft"
	CaseTrusted     CaseState = "trusted"
	CaseRejected    CaseState = "rejected"
	CaseWithdrawn   CaseState = "withdrawn"
	CaseInvalidated CaseState = "invalidated"
	CaseDuplicate   CaseState = "duplicate"
	CaseArchived    CaseState = "archived"
)

// SolutionState is the state of a solution revision.
type SolutionState string

const (
	SolutionProposed  SolutionState = "proposed"
	SolutionTrusted   SolutionState = "trusted"
	SolutionRejected  SolutionState = "rejected"
	SolutionWithdrawn SolutionState = "withdrawn"
)

// MembershipState is the state of a ticket's Issue membership.
type MembershipState string

const (
	MembershipTentative MembershipState = "tentative"
	MembershipDetached  MembershipState = "detached"
)

// ApplicationState is the state of the application of a reused solution, and of
// each review of it.
type ApplicationState string

const (
	ApplicationProposed  ApplicationState = "proposed"
	ApplicationAccepted  ApplicationState = "accepted"
	ApplicationRejected  ApplicationState = "rejected"
	ApplicationWithdrawn ApplicationState = "withdrawn"
)

// Allowed state transitions, per entity: from each state (the empty key is a
// new entity) to the states an Update may move it to. The test guard fails
// any committed change not listed here, so a new path through a state
// machine is a deliberate edit to this table. Production does not check it.

var investigationTransitions = transitions[InvestigationState]{
	"":                                 {InvestigationInvestigating, InvestigationQueued},
	InvestigationAgentProposed:         {InvestigationTrusted, InvestigationWithdrawn},
	InvestigationApplied:               {InvestigationWithdrawn},
	InvestigationAwaitingConfiguration: {InvestigationRunning, InvestigationSuperseded},
	InvestigationInvestigating: {
		InvestigationAgentProposed, InvestigationFailed, InvestigationSuperseded,
	},
	InvestigationQueued: {
		InvestigationAwaitingConfiguration, InvestigationAwaitingForward, InvestigationInvestigating,
		InvestigationRunning, InvestigationSuperseded,
	},
	InvestigationReuseProposed: {InvestigationApplied, InvestigationWithdrawn},
	// Running returns to Queued when the provider's usage limit refuses routing.
	InvestigationRunning: {
		InvestigationAgentProposed, InvestigationAwaitingInformation, InvestigationFailed,
		InvestigationInvestigating, InvestigationJoined, InvestigationQueued, InvestigationReturnedToSupport,
		InvestigationReuseProposed, InvestigationSuperseded,
	},
	InvestigationTrusted: {InvestigationRejected, InvestigationWithdrawn},
}

var runTransitions = transitions[RunState]{
	"":                       {RunQueued},
	RunAwaitingConfiguration: {RunRunning, RunSuperseded},
	RunAwaitingReview:        {RunCompleted, RunInvalidated, RunRestricted},
	RunCompleted:             {RunInvalidated, RunRejected},
	RunInvestigating:         {RunAwaitingReview, RunFailed, RunRestricted},
	RunQueued: {
		RunAwaitingConfiguration, RunAwaitingForward, RunInvestigating, RunRestricted, RunRunning,
		RunSuperseded,
	},
	RunRestricted: {RunAwaitingReview},
	// Running returns to Queued when the provider's usage limit refuses routing.
	RunRunning: {
		RunAwaitingInformation, RunAwaitingReview, RunFailed, RunInvestigating, RunJoined,
		RunQueued, RunReturnedToSupport, RunSuperseded,
	},
}

var caseTransitions = transitions[CaseState]{
	"":          {CaseArchived, CaseDraft, CaseDuplicate, CaseTrusted},
	CaseDraft:   {CaseInvalidated, CaseTrusted},
	CaseTrusted: {CaseInvalidated, CaseRejected, CaseWithdrawn},
}

var solutionTransitions = transitions[SolutionState]{
	"": {SolutionProposed, SolutionRejected, SolutionTrusted, SolutionWithdrawn},
}

var membershipTransitions = transitions[MembershipState]{
	"":                  {MembershipTentative},
	MembershipTentative: {MembershipDetached},
}

var applicationTransitions = transitions[ApplicationState]{
	"":                  {ApplicationProposed},
	ApplicationAccepted: {ApplicationWithdrawn},
	ApplicationProposed: {ApplicationAccepted, ApplicationWithdrawn},
}

// transitions maps a state to the states it may change to.
type transitions[S ~string] map[S][]S

func (t transitions[S]) allows(from, to S) bool {
	for _, allowed := range t[from] {
		if allowed == to {
			return true
		}
	}
	return false
}
