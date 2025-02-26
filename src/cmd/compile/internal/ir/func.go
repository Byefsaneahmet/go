// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ir

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/src"
	"fmt"
	"strings"
)

// A Func corresponds to a single function in a Go program
// (and vice versa: each function is denoted by exactly one *Func).
//
// There are multiple nodes that represent a Func in the IR.
//
// The ONAME node (Func.Nname) is used for plain references to it.
// The ODCLFUNC node (the Func itself) is used for its declaration code.
// The OCLOSURE node (Func.OClosure) is used for a reference to a
// function literal.
//
// An imported function will have an ONAME node which points to a Func
// with an empty body.
// A declared function or method has an ODCLFUNC (the Func itself) and an ONAME.
// A function literal is represented directly by an OCLOSURE, but it also
// has an ODCLFUNC (and a matching ONAME) representing the compiled
// underlying form of the closure, which accesses the captured variables
// using a special data structure passed in a register.
//
// A method declaration is represented like functions, except f.Sym
// will be the qualified method name (e.g., "T.m").
//
// A method expression (T.M) is represented as an OMETHEXPR node,
// in which n.Left and n.Right point to the type and method, respectively.
// Each distinct mention of a method expression in the source code
// constructs a fresh node.
//
// A method value (t.M) is represented by ODOTMETH/ODOTINTER
// when it is called directly and by OMETHVALUE otherwise.
// These are like method expressions, except that for ODOTMETH/ODOTINTER,
// the method name is stored in Sym instead of Right.
// Each OMETHVALUE ends up being implemented as a new
// function, a bit like a closure, with its own ODCLFUNC.
// The OMETHVALUE uses n.Func to record the linkage to
// the generated ODCLFUNC, but there is no
// pointer from the Func back to the OMETHVALUE.
type Func struct {
	miniNode
	Body Nodes

	Nname    *Name        // ONAME node
	OClosure *ClosureExpr // OCLOSURE node

	// Extra entry code for the function. For example, allocate and initialize
	// memory for escaping parameters.
	Enter Nodes
	Exit  Nodes

	// ONAME nodes for all params/locals for this func/closure, does NOT
	// include closurevars until transforming closures during walk.
	// Names must be listed PPARAMs, PPARAMOUTs, then PAUTOs,
	// with PPARAMs and PPARAMOUTs in order corresponding to the function signature.
	// However, as anonymous or blank PPARAMs are not actually declared,
	// they are omitted from Dcl.
	// Anonymous and blank PPARAMOUTs are declared as ~rNN and ~bNN Names, respectively.
	Dcl []*Name

	// ClosureVars lists the free variables that are used within a
	// function literal, but formally declared in an enclosing
	// function. The variables in this slice are the closure function's
	// own copy of the variables, which are used within its function
	// body. They will also each have IsClosureVar set, and will have
	// Byval set if they're captured by value.
	ClosureVars []*Name

	// Enclosed functions that need to be compiled.
	// Populated during walk.
	Closures []*Func

	// Parents records the parent scope of each scope within a
	// function. The root scope (0) has no parent, so the i'th
	// scope's parent is stored at Parents[i-1].
	Parents []ScopeID

	// Marks records scope boundary changes.
	Marks []Mark

	FieldTrack map[*obj.LSym]struct{}
	DebugInfo  interface{}
	LSym       *obj.LSym // Linker object in this function's native ABI (Func.ABI)

	Inl *Inline

	// funcLitGen and goDeferGen track how many closures have been
	// created in this function for function literals and go/defer
	// wrappers, respectively. Used by closureName for creating unique
	// function names.
	//
	// Tracking goDeferGen separately avoids wrappers throwing off
	// function literal numbering (e.g., runtime/trace_test.TestTraceSymbolize.func11).
	funcLitGen int32
	goDeferGen int32

	Label int32 // largest auto-generated label in this function

	Endlineno src.XPos
	WBPos     src.XPos // position of first write barrier; see SetWBPos

	Pragma PragmaFlag // go:xxx function annotations

	flags bitset16

	// ABI is a function's "definition" ABI. This is the ABI that
	// this function's generated code is expecting to be called by.
	//
	// For most functions, this will be obj.ABIInternal. It may be
	// a different ABI for functions defined in assembly or ABI wrappers.
	//
	// This is included in the export data and tracked across packages.
	ABI obj.ABI
	// ABIRefs is the set of ABIs by which this function is referenced.
	// For ABIs other than this function's definition ABI, the
	// compiler generates ABI wrapper functions. This is only tracked
	// within a package.
	ABIRefs obj.ABISet

	NumDefers  int32 // number of defer calls in the function
	NumReturns int32 // number of explicit returns in the function

	// nwbrCalls records the LSyms of functions called by this
	// function for go:nowritebarrierrec analysis. Only filled in
	// if nowritebarrierrecCheck != nil.
	NWBRCalls *[]SymAndPos

	// For wrapper functions, WrappedFunc point to the original Func.
	// Currently only used for go/defer wrappers.
	WrappedFunc *Func

	// WasmImport is used by the //go:wasmimport directive to store info about
	// a WebAssembly function import.
	WasmImport *WasmImport
}

// WasmImport stores metadata associated with the //go:wasmimport pragma.
type WasmImport struct {
	Module string
	Name   string
}

// NewFunc returns a new Func with the given name and type.
//
// fpos is the position of the "func" token, and npos is the position
// of the name identifier.
//
// TODO(mdempsky): I suspect there's no need for separate fpos and
// npos.
func NewFunc(fpos, npos src.XPos, sym *types.Sym, typ *types.Type) *Func {
	name := NewNameAt(npos, sym, typ)
	name.Class = PFUNC
	sym.SetFunc(true)

	fn := &Func{Nname: name}
	fn.pos = fpos
	fn.op = ODCLFUNC
	// Most functions are ABIInternal. The importer or symabis
	// pass may override this.
	fn.ABI = obj.ABIInternal
	fn.SetTypecheck(1)

	name.Func = fn

	return fn
}

func (f *Func) isStmt() {}

func (n *Func) copy() Node                                  { panic(n.no("copy")) }
func (n *Func) doChildren(do func(Node) bool) bool          { return doNodes(n.Body, do) }
func (n *Func) editChildren(edit func(Node) Node)           { editNodes(n.Body, edit) }
func (n *Func) editChildrenWithHidden(edit func(Node) Node) { editNodes(n.Body, edit) }

func (f *Func) Type() *types.Type                { return f.Nname.Type() }
func (f *Func) Sym() *types.Sym                  { return f.Nname.Sym() }
func (f *Func) Linksym() *obj.LSym               { return f.Nname.Linksym() }
func (f *Func) LinksymABI(abi obj.ABI) *obj.LSym { return f.Nname.LinksymABI(abi) }

// An Inline holds fields used for function bodies that can be inlined.
type Inline struct {
	Cost int32 // heuristic cost of inlining this function

	// Copy of Func.Dcl for use during inlining. This copy is needed
	// because the function's Dcl may change from later compiler
	// transformations. This field is also populated when a function
	// from another package is imported and inlined.
	Dcl     []*Name
	HaveDcl bool // whether we've loaded Dcl

	// CanDelayResults reports whether it's safe for the inliner to delay
	// initializing the result parameters until immediately before the
	// "return" statement.
	CanDelayResults bool
}

// A Mark represents a scope boundary.
type Mark struct {
	// Pos is the position of the token that marks the scope
	// change.
	Pos src.XPos

	// Scope identifies the innermost scope to the right of Pos.
	Scope ScopeID
}

// A ScopeID represents a lexical scope within a function.
type ScopeID int32

const (
	funcDupok         = 1 << iota // duplicate definitions ok
	funcWrapper                   // hide frame from users (elide in tracebacks, don't count as a frame for recover())
	funcABIWrapper                // is an ABI wrapper (also set flagWrapper)
	funcNeedctxt                  // function uses context register (has closure variables)
	funcReflectMethod             // function calls reflect.Type.Method or MethodByName
	// true if closure inside a function; false if a simple function or a
	// closure in a global variable initialization
	funcIsHiddenClosure
	funcIsDeadcodeClosure        // true if closure is deadcode
	funcHasDefer                 // contains a defer statement
	funcNilCheckDisabled         // disable nil checks when compiling this function
	funcInlinabilityChecked      // inliner has already determined whether the function is inlinable
	funcNeverReturns             // function never returns (in most cases calls panic(), os.Exit(), or equivalent)
	funcInstrumentBody           // add race/msan/asan instrumentation during SSA construction
	funcOpenCodedDeferDisallowed // can't do open-coded defers
	funcClosureResultsLost       // closure is called indirectly and we lost track of its results; used by escape analysis
	funcPackageInit              // compiler emitted .init func for package
)

type SymAndPos struct {
	Sym *obj.LSym // LSym of callee
	Pos src.XPos  // line of call
}

func (f *Func) Dupok() bool                    { return f.flags&funcDupok != 0 }
func (f *Func) Wrapper() bool                  { return f.flags&funcWrapper != 0 }
func (f *Func) ABIWrapper() bool               { return f.flags&funcABIWrapper != 0 }
func (f *Func) Needctxt() bool                 { return f.flags&funcNeedctxt != 0 }
func (f *Func) ReflectMethod() bool            { return f.flags&funcReflectMethod != 0 }
func (f *Func) IsHiddenClosure() bool          { return f.flags&funcIsHiddenClosure != 0 }
func (f *Func) IsDeadcodeClosure() bool        { return f.flags&funcIsDeadcodeClosure != 0 }
func (f *Func) HasDefer() bool                 { return f.flags&funcHasDefer != 0 }
func (f *Func) NilCheckDisabled() bool         { return f.flags&funcNilCheckDisabled != 0 }
func (f *Func) InlinabilityChecked() bool      { return f.flags&funcInlinabilityChecked != 0 }
func (f *Func) NeverReturns() bool             { return f.flags&funcNeverReturns != 0 }
func (f *Func) InstrumentBody() bool           { return f.flags&funcInstrumentBody != 0 }
func (f *Func) OpenCodedDeferDisallowed() bool { return f.flags&funcOpenCodedDeferDisallowed != 0 }
func (f *Func) ClosureResultsLost() bool       { return f.flags&funcClosureResultsLost != 0 }
func (f *Func) IsPackageInit() bool            { return f.flags&funcPackageInit != 0 }

func (f *Func) SetDupok(b bool)                    { f.flags.set(funcDupok, b) }
func (f *Func) SetWrapper(b bool)                  { f.flags.set(funcWrapper, b) }
func (f *Func) SetABIWrapper(b bool)               { f.flags.set(funcABIWrapper, b) }
func (f *Func) SetNeedctxt(b bool)                 { f.flags.set(funcNeedctxt, b) }
func (f *Func) SetReflectMethod(b bool)            { f.flags.set(funcReflectMethod, b) }
func (f *Func) SetIsHiddenClosure(b bool)          { f.flags.set(funcIsHiddenClosure, b) }
func (f *Func) SetIsDeadcodeClosure(b bool)        { f.flags.set(funcIsDeadcodeClosure, b) }
func (f *Func) SetHasDefer(b bool)                 { f.flags.set(funcHasDefer, b) }
func (f *Func) SetNilCheckDisabled(b bool)         { f.flags.set(funcNilCheckDisabled, b) }
func (f *Func) SetInlinabilityChecked(b bool)      { f.flags.set(funcInlinabilityChecked, b) }
func (f *Func) SetNeverReturns(b bool)             { f.flags.set(funcNeverReturns, b) }
func (f *Func) SetInstrumentBody(b bool)           { f.flags.set(funcInstrumentBody, b) }
func (f *Func) SetOpenCodedDeferDisallowed(b bool) { f.flags.set(funcOpenCodedDeferDisallowed, b) }
func (f *Func) SetClosureResultsLost(b bool)       { f.flags.set(funcClosureResultsLost, b) }
func (f *Func) SetIsPackageInit(b bool)            { f.flags.set(funcPackageInit, b) }

func (f *Func) SetWBPos(pos src.XPos) {
	if base.Debug.WB != 0 {
		base.WarnfAt(pos, "write barrier")
	}
	if !f.WBPos.IsKnown() {
		f.WBPos = pos
	}
}

// FuncName returns the name (without the package) of the function f.
func FuncName(f *Func) string {
	if f == nil || f.Nname == nil {
		return "<nil>"
	}
	return f.Sym().Name
}

// PkgFuncName returns the name of the function referenced by f, with package
// prepended.
//
// This differs from the compiler's internal convention where local functions
// lack a package. This is primarily useful when the ultimate consumer of this
// is a human looking at message.
func PkgFuncName(f *Func) string {
	if f == nil || f.Nname == nil {
		return "<nil>"
	}
	s := f.Sym()
	pkg := s.Pkg

	return pkg.Path + "." + s.Name
}

// LinkFuncName returns the name of the function f, as it will appear in the
// symbol table of the final linked binary.
func LinkFuncName(f *Func) string {
	if f == nil || f.Nname == nil {
		return "<nil>"
	}
	s := f.Sym()
	pkg := s.Pkg

	return objabi.PathToPrefix(pkg.Path) + "." + s.Name
}

var CurFunc *Func

// WithFunc invokes do with CurFunc and base.Pos set to curfn and
// curfn.Pos(), respectively, and then restores their previous values
// before returning.
func WithFunc(curfn *Func, do func()) {
	oldfn, oldpos := CurFunc, base.Pos
	defer func() { CurFunc, base.Pos = oldfn, oldpos }()

	CurFunc, base.Pos = curfn, curfn.Pos()
	do()
}

func FuncSymName(s *types.Sym) string {
	return s.Name + "·f"
}

// ClosureDebugRuntimeCheck applies boilerplate checks for debug flags
// and compiling runtime.
func ClosureDebugRuntimeCheck(clo *ClosureExpr) {
	if base.Debug.Closure > 0 {
		if clo.Esc() == EscHeap {
			base.WarnfAt(clo.Pos(), "heap closure, captured vars = %v", clo.Func.ClosureVars)
		} else {
			base.WarnfAt(clo.Pos(), "stack closure, captured vars = %v", clo.Func.ClosureVars)
		}
	}
	if base.Flag.CompilingRuntime && clo.Esc() == EscHeap && !clo.IsGoWrap {
		base.ErrorfAt(clo.Pos(), 0, "heap-allocated closure %s, not allowed in runtime", FuncName(clo.Func))
	}
}

// IsTrivialClosure reports whether closure clo has an
// empty list of captured vars.
func IsTrivialClosure(clo *ClosureExpr) bool {
	return len(clo.Func.ClosureVars) == 0
}

// globClosgen is like Func.Closgen, but for the global scope.
var globClosgen int32

// closureName generates a new unique name for a closure within outerfn at pos.
func closureName(outerfn *Func, pos src.XPos, why Op) *types.Sym {
	pkg := types.LocalPkg
	outer := "glob."
	var prefix string
	switch why {
	default:
		base.FatalfAt(pos, "closureName: bad Op: %v", why)
	case OCLOSURE:
		if outerfn == nil || outerfn.OClosure == nil {
			prefix = "func"
		}
	case OGO:
		prefix = "gowrap"
	case ODEFER:
		prefix = "deferwrap"
	}
	gen := &globClosgen

	// There may be multiple functions named "_". In those
	// cases, we can't use their individual Closgens as it
	// would lead to name clashes.
	if outerfn != nil && !IsBlank(outerfn.Nname) {
		pkg = outerfn.Sym().Pkg
		outer = FuncName(outerfn)

		if why == OCLOSURE {
			gen = &outerfn.funcLitGen
		} else {
			gen = &outerfn.goDeferGen
		}
	}

	// If this closure was created due to inlining, then incorporate any
	// inlined functions' names into the closure's linker symbol name
	// too (#60324).
	if inlIndex := base.Ctxt.InnermostPos(pos).Base().InliningIndex(); inlIndex >= 0 {
		names := []string{outer}
		base.Ctxt.InlTree.AllParents(inlIndex, func(call obj.InlinedCall) {
			names = append(names, call.Name)
		})
		outer = strings.Join(names, ".")
	}

	*gen++
	return pkg.Lookup(fmt.Sprintf("%s.%s%d", outer, prefix, *gen))
}

// NewClosureFunc creates a new Func to represent a function literal
// with the given type.
//
// fpos the position used for the underlying ODCLFUNC and ONAME,
// whereas cpos is the position used for the OCLOSURE. They're
// separate because in the presence of inlining, the OCLOSURE node
// should have an inline-adjusted position, whereas the ODCLFUNC and
// ONAME must not.
//
// outerfn is the enclosing function, if any. The returned function is
// appending to pkg.Funcs.
//
// why is the reason we're generating this Func. It can be OCLOSURE
// (for a normal function literal) or OGO or ODEFER (for wrapping a
// call expression that has parameters or results).
func NewClosureFunc(fpos, cpos src.XPos, why Op, typ *types.Type, outerfn *Func, pkg *Package) *Func {
	fn := NewFunc(fpos, fpos, closureName(outerfn, cpos, why), typ)
	fn.SetIsHiddenClosure(outerfn != nil)

	clo := &ClosureExpr{Func: fn}
	clo.op = OCLOSURE
	clo.pos = cpos
	clo.SetType(typ)
	clo.SetTypecheck(1)
	fn.OClosure = clo

	fn.Nname.Defn = fn
	pkg.Funcs = append(pkg.Funcs, fn)

	return fn
}
