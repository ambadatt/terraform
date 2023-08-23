package stackeval

import (
	"context"
	"fmt"
	"sync"

	"github.com/hashicorp/go-slug/sourcebundle"
	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/instances"
	"github.com/hashicorp/terraform/internal/promising"
	"github.com/hashicorp/terraform/internal/stacks/stackaddrs"
	"github.com/hashicorp/terraform/internal/stacks/stackconfig"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

// Main is the central node of all data required for performing the major
// actions against a stack: validation, planning, and applying.
//
// This type delegates to various other types in this package to implement
// the real logic, with Main focused on enabling the collaboration between
// objects of those other types.
type Main struct {
	config *stackconfig.Config

	// validating captures the data needed when validating configuration,
	// in which case we consider _only_ the configuration and don't take
	// into account any existing state or specific input variable values.
	validating *mainValidating

	// planning captures the data needed when creating or applying a plan,
	// but which need not be populated when only using the validation-related
	// functionality of this package.
	planning *mainPlanning

	// applying captures the data needed when applying a plan. This can
	// only be populated if "planning" is also populated, because the process
	// of applying includes re-evaluation of plans based on new information
	// gathered during the apply process.
	applying *mainApplying

	// providerFactories is a set of callback functions through which the
	// runtime can obtain new instances of each of the available providers.
	providerFactories ProviderFactories

	// The remaining fields memoize other objects we might create in response
	// to method calls. Must lock "mu" before interacting with them.
	mu              sync.Mutex
	mainStackConfig *StackConfig
	mainStack       *Stack
	providerTypes   map[addrs.Provider]*ProviderType
	cleanupFuncs    []func(context.Context) tfdiags.Diagnostics
}

var _ namedPromiseReporter = (*Main)(nil)

type mainValidating struct {
	opts ValidateOpts
}

type mainPlanning struct {
	opts PlanOpts
}

type mainApplying struct {
	opts ApplyOpts
}

func NewForValidating(config *stackconfig.Config, opts ValidateOpts) *Main {
	return &Main{
		config: config,
		validating: &mainValidating{
			opts: opts,
		},
		providerFactories: opts.ProviderFactories,
		providerTypes:     make(map[addrs.Provider]*ProviderType),
	}
}

func NewForPlanning(config *stackconfig.Config, opts PlanOpts) *Main {
	// TODO: This function should also take an optional prior state, but we
	// don't actually have a type for that yet.

	return &Main{
		config: config,
		planning: &mainPlanning{
			opts: opts,
		},
		providerFactories: opts.ProviderFactories,
		providerTypes:     make(map[addrs.Provider]*ProviderType),
	}
}

// Validating returns true if the receiving [Main] is configured for validating.
//
// If this returns false then validation methods may panic or return strange
// results.
func (m *Main) Validating() bool {
	return m.validating != nil
}

// Planning returns true if the receiving [Main] is configured for planning.
//
// If this returns false then planning methods may panic or return strange
// results.
func (m *Main) Planning() bool {
	return m.planning != nil
}

// Applying returns true if the receiving [Main] is configured for applying.
//
// If this returns false then applying methods may panic or return strange
// results.
func (m *Main) Applying() bool {
	return m.applying != nil && m.Planning()
}

// PlanningMode returns the planning options to use during the planning phase,
// or panics if this [Main] was not instantiated for planning.
//
// Do not modify anything reachable through the returned pointer.
func (m *Main) PlanningOpts() *PlanOpts {
	if !m.Planning() {
		panic("stacks language runtime is not instantiated for planning")
	}
	return &m.planning.opts
}

// SourceBundle returns the source code bundle that the stack configuration
// was originally loaded from and that should also contain the source code
// for any modules that "component" blocks refer to.
func (m *Main) SourceBundle(ctx context.Context) *sourcebundle.Bundle {
	return m.config.Sources
}

// MainStackConfig returns the [StackConfig] object representing the main
// stack configuration, which is at the root of the configuration tree.
//
// This represents the static configuration. The main stack configuration
// always has exactly one "dynamic" instance, which you can access by
// calling [Main.MainStack] instead. The static configuration object is used
// for validation, but plan and apply both use the stack instance.
func (m *Main) MainStackConfig(ctx context.Context) *StackConfig {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mainStackConfig == nil {
		m.mainStackConfig = newStackConfig(m, stackaddrs.RootStack, m.config.Root)
	}
	return m.mainStackConfig
}

// MainStack returns the [Stack] object representing the main stack, which
// is the root of the configuration tree.
func (m *Main) MainStack(ctx context.Context) *Stack {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.mainStack == nil {
		m.mainStack = newStack(m, stackaddrs.RootStackInstance)
	}
	return m.mainStack
}

// StackConfig returns the [StackConfig] object representing the stack with
// the given address, or nil if there is no such stack.
func (m *Main) StackConfig(ctx context.Context, addr stackaddrs.Stack) *StackConfig {
	ret := m.MainStackConfig(ctx)
	for _, step := range addr {
		ret = ret.ChildConfig(ctx, step)
		if ret == nil {
			return nil
		}
	}
	return ret
}

// StackUnchecked returns the [Stack] object representing the stack instance
// with the given address, or nil if the address traverses through an embedded
// stack call that doesn't exist at all.
//
// This function cannot check whether the instance keys in the path correspond
// to instances actually declared by the configuration. If you need to check
// that use [Main.Stack] instead, but consider the additional overhead that
// extra checking implies.
func (m *Main) StackUnchecked(ctx context.Context, addr stackaddrs.StackInstance) *Stack {
	ret := m.MainStack(ctx)
	for _, step := range addr {
		ret = ret.ChildStackUnchecked(ctx, step)
		if ret == nil {
			return nil
		}
	}
	return ret
}

// Stack returns the [Stack] object representing the stack instance with the
// given address, or nil if we know for certain that such a stack instance
// is not declared in the configuration.
//
// This is like [Main.StackUnchecked] but additionally checks whether all of
// the instance keys in the stack instance path correspond to instances declared
// by for_each arguments (or lack thereof) in the configuration. This involves
// evaluating all of the "for_each" expressions, and so will block on whatever
// those expressions depend on.
//
// If any of the stack calls along the path have an as-yet-unknown set of
// instances, this function will optimistically return a non-nil stack but
// further operations with that stack are likely to return unknown values
// themselves.
//
// If you know you are holding a [stackaddrs.StackInstance] that was from
// a valid [Stack] previously returned (directly or indirectly) then you can
// avoid the additional overhead by using [Main.StackUnchecked] instead.
func (m *Main) Stack(ctx context.Context, addr stackaddrs.StackInstance, phase EvalPhase) *Stack {
	ret := m.MainStack(ctx)
	for _, step := range addr {
		ret = ret.ChildStackChecked(ctx, step, phase)
		if ret == nil {
			return nil
		}
	}
	return ret
}

// ProviderFactories returns the collection of factory functions for providers
// that are available to this instance of the evaluation runtime.
func (m *Main) ProviderFactories() ProviderFactories {
	return m.providerFactories
}

// ProviderType returns the [ProviderType] object representing the given
// provider source address.
//
// This does not check whether the given provider type is available in the
// current evaluation context, but attempting to create a client for a
// provider that isn't available will return an error at startup time.
func (m *Main) ProviderType(ctx context.Context, addr addrs.Provider) *ProviderType {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.providerTypes[addr] == nil {
		m.providerTypes[addr] = newProviderType(m, addr)
	}
	return m.providerTypes[addr]
}

// ProviderInstance returns the provider instance with the given address,
// or nil if there is no such provider instance.
//
// This function needs to evaluate the for_each expression of each stack along
// the path and of a final multi-instance provider configuration, and so will
// block on whatever those expressions depend on.
//
// If any of the objects along the path have an as-yet-unknown set of
// instances, this function will optimistically return a non-nil provider
// configuration but further operations with that configuration are likely
// to return unknown values themselves.
func (m *Main) ProviderInstance(ctx context.Context, addr stackaddrs.AbsProviderConfigInstance, phase EvalPhase) *ProviderInstance {
	stack := m.Stack(ctx, addr.Stack, phase)
	if stack == nil {
		return nil
	}
	provider := stack.Provider(ctx, addr.Item.ProviderConfig)
	if provider == nil {
		return nil
	}
	insts := provider.Instances(ctx, phase)
	if insts == nil {
		// A nil result means that the for_each expression is unknown, and
		// so we must optimistically return an instance referring to the
		// given address which will then presumably yield unknown values
		// of some kind when used.
		return newProviderInstance(provider, addr.Item.Key, instances.RepetitionData{
			EachKey:   cty.UnknownVal(cty.String),
			EachValue: cty.DynamicVal,
		})
	}
	return insts[addr.Item.Key]
}

func (m *Main) RootVariableValue(ctx context.Context, addr stackaddrs.InputVariable, phase EvalPhase) ExternalInputValue {
	switch phase {
	case PlanPhase:
		if m.planning == nil {
			panic("using plan-phase input variable values when not configured for planning")
		}
		ret, ok := m.planning.opts.InputVariableValues[addr]
		if !ok {
			return ExternalInputValue{
				Value: cty.NullVal(cty.DynamicPseudoType),
			}
		}
		return ret
	default:
		// Root input variable values are not available in any other phase.
		return ExternalInputValue{
			Value: cty.DynamicVal, // placeholder value
		}
	}
}

// RegisterCleanup registers an arbitrary callback function to run when a
// walk driver eventually calls [Main.RunCleanup] on the same receiver.
//
// This is intended for cleaning up any resources that would not naturally
// be cleaned up as a result of garbage-collecting the [Main] object and its
// many descendents.
//
// The context passed to a callback function may be already cancelled by the
// time the callback is running, if the cleanup is running in response to
// cancellation.
func (m *Main) RegisterCleanup(cb func(ctx context.Context) tfdiags.Diagnostics) {
	m.mu.Lock()
	m.cleanupFuncs = append(m.cleanupFuncs, cb)
	m.mu.Unlock()
}

// DoCleanup executes any cleanup functions previously registered using
// [Main.RegisterCleanup], returning any collected diagnostics.
//
// Call this only once evaluation has completed and there aren't any requests
// outstanding that might be using resources that this will free. After calling
// this, the [Main] and all other objects created through it become invalid
// and must not be used anymore.
func (m *Main) DoCleanup(ctx context.Context) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	m.mu.Lock()
	funcs := m.cleanupFuncs
	m.cleanupFuncs = nil
	m.mu.Unlock()
	for _, cb := range funcs {
		diags = diags.Append(
			cb(ctx),
		)
	}
	return diags
}

// mustStackConfig is like [Main.StackConfig] except that it panics if it
// does not find a stack configuration object matching the given address,
// for situations where the absense of a stack config represents a bug
// somewhere in Terraform, rather than incorrect user input.
func (m *Main) mustStackConfig(ctx context.Context, addr stackaddrs.Stack) *StackConfig {
	ret := m.StackConfig(ctx, addr)
	if ret == nil {
		panic(fmt.Sprintf("no configuration for %s", addr))
	}
	return ret
}

// StackCallConfig returns the [StackCallConfig] object representing the
// "stack" block in the configuration with the given address, or nil if there
// is no such block.
func (m *Main) StackCallConfig(ctx context.Context, addr stackaddrs.ConfigStackCall) *StackCallConfig {
	caller := m.StackConfig(ctx, addr.Stack)
	if caller == nil {
		return nil
	}
	return caller.StackCall(ctx, addr.Item)
}

// reportNamedPromises implements namedPromiseReporter.
func (m *Main) reportNamedPromises(cb func(id promising.PromiseID, name string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mainStackConfig != nil {
		m.mainStackConfig.reportNamedPromises(cb)
	}
}
