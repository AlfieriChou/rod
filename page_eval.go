package rod

import (
	"fmt"
	"time"

	"github.com/go-rod/rod/lib/assets"
	"github.com/go-rod/rod/lib/assets/js"
	"github.com/go-rod/rod/lib/proto"
	"github.com/go-rod/rod/lib/utils"
	"github.com/ysmood/gson"
)

// Eval options for Page.Evaluate
type Eval struct {
	// If enabled the eval result will be a plain JSON value.
	// If disabled the eval result will be a reference of a remote js object.
	ByValue bool

	// ThisObj represents the "this" object in the JS
	ThisObj *proto.RuntimeRemoteObject

	// JS code to eval
	JS string

	// JSArgs represents the arguments in the JS if the JS is a function definition.
	// If an argument is *proto.RuntimeRemoteObject type, the corresponding remote object will be used.
	// Or it will be passed as a plain JSON value.
	JSArgs []interface{}

	// Whether execution should be treated as initiated by user in the UI.
	UserGesture bool

	jsHelper bool
}

// NewEval options. ByValue will be set to true.
func NewEval(js string, args ...interface{}) *Eval {
	return &Eval{true, nil, js, args, false, false}
}

// This set the obj as ThisObj
func (e *Eval) This(obj *proto.RuntimeRemoteObject) *Eval {
	e.ThisObj = obj
	return e
}

// ByObject disables ByValue.
func (e *Eval) ByObject() *Eval {
	e.ByValue = false
	return e
}

// ByUser enables UserGesture.
func (e *Eval) ByUser() *Eval {
	e.UserGesture = true
	return e
}

// Strings appends each string to JSArgs
func (e *Eval) Strings(list ...string) *Eval {
	for _, s := range list {
		e.JSArgs = append(e.JSArgs, s)
	}
	return e
}

func (e *Eval) formatToJSFunc() string {
	if detectJSFunction(e.JS) {
		return fmt.Sprintf(`function() { return (%s).apply(this, arguments) }`, e.JS)
	}
	return fmt.Sprintf(`function() { return %s }`, e.JS)
}

// Convert name and jsArgs to Page.Eval, the name is method name in the "lib/assets/helper.js".
func jsHelper(name js.Name, args ...interface{}) *Eval {
	return &Eval{
		ByValue:  true,
		JS:       fmt.Sprintf(`(rod, ...args) => rod.%s.apply(this, args)`, name),
		JSArgs:   args,
		jsHelper: true,
	}
}

// Eval js on the page. It's just a shortcut for Page.Evaluate.
func (p *Page) Eval(js string, jsArgs ...interface{}) (*proto.RuntimeRemoteObject, error) {
	return p.Evaluate(NewEval(js, jsArgs...))
}

// Evaluate js on the page.
func (p *Page) Evaluate(opts *Eval) (res *proto.RuntimeRemoteObject, err error) {
	backoff := utils.BackoffSleeper(30*time.Millisecond, 3*time.Second, nil)

	// js context will be invalid if a frame is reloaded or not ready, then the isNilContextErr
	// will be true, then we retry the eval again.
	for {
		p.jsContextIDLock.Lock()
		wait := p.jsContextIDWait
		p.jsContextIDLock.Unlock()

		select {
		case <-p.ctx.Done():
		case <-wait:
		}

		res, err = p.evaluate(opts)
		if isNilContextErr(err) {
			backoff(p.ctx)
			continue
		}
		return
	}
}

func (p *Page) evaluate(opts *Eval) (*proto.RuntimeRemoteObject, error) {
	args, err := p.formatArgs(opts)
	if err != nil {
		return nil, err
	}

	req := proto.RuntimeCallFunctionOn{
		AwaitPromise:        true,
		ReturnByValue:       opts.ByValue,
		UserGesture:         opts.UserGesture,
		FunctionDeclaration: opts.formatToJSFunc(),
		Arguments:           args,
	}

	if opts.ThisObj == nil {
		req.ExecutionContextID = p.getJSContextID()
	} else {
		req.ObjectID = opts.ThisObj.ObjectID
	}

	res, err := req.Call(p)
	if err != nil {
		return nil, err
	}

	if res.ExceptionDetails != nil {
		return nil, &ErrEval{res.ExceptionDetails}
	}

	return res.Result, nil
}

func (p *Page) initSession() error {
	obj, err := proto.TargetAttachToTarget{
		TargetID: p.TargetID,
		Flatten:  true, // if it's not set no response will return
	}.Call(p)
	if err != nil {
		return err
	}
	p.SessionID = obj.SessionID

	// If we don't enable it, it will cause a lot of unexpected browser behavior.
	// Such as proto.PageAddScriptToEvaluateOnNewDocument won't work.
	p.EnableDomain(&proto.PageEnable{})

	// If we don't enable it, it will remove remote node id whenever we disable the domain
	// even after we re-enable it again we can't query the ids any more.
	p.EnableDomain(&proto.DOMEnable{})

	p.FrameID = proto.PageFrameID(p.TargetID)

	p.guardJSContext()

	p.EnableDomain(&proto.RuntimeEnable{})

	return nil
}

func (p *Page) getJSContextID() proto.RuntimeExecutionContextID {
	p.jsContextIDLock.Lock()
	defer p.jsContextIDLock.Unlock()
	return p.jsContextIDs[p.FrameID]
}

// sync RuntimeExecutionContextID with remote
func (p *Page) guardJSContext() {
	events := p.Event()
	go func() {
		for event := range events {
			switch e := event.(type) {
			case *proto.RuntimeExecutionContextCreated:
				p.jsContextIDLock.Lock()
				frameID := proto.PageFrameID(e.Context.AuxData["frameId"].Str())
				p.jsContextIDs[frameID] = e.Context.ID
				if frameID == p.FrameID {
					close(p.jsContextIDWait)
				}
				p.jsContextIDLock.Unlock()

			case *proto.PageFrameStartedLoading:
				p.jsContextIDLock.Lock()
				if e.FrameID == p.FrameID {
					p.jsContextIDWait = make(chan struct{})
				}
				delete(p.jsContextIDs, e.FrameID)
				p.jsContextIDLock.Unlock()
			}
		}
	}()
}

// We must pass the jsHelper right before we eval it, or the jsHelper may not be generated yet,
// we only inject the js helper on the first Page.EvalWithOption .
func (p *Page) formatArgs(opts *Eval) ([]*proto.RuntimeCallArgument, error) {
	var jsArgs []interface{}

	if opts.jsHelper {
		helper, err := p.getJSHelperObj()
		if err != nil {
			return nil, err
		}
		jsArgs = append([]interface{}{helper}, opts.JSArgs...)
	} else {
		jsArgs = opts.JSArgs
	}

	formated := []*proto.RuntimeCallArgument{}
	for _, arg := range jsArgs {
		if obj, ok := arg.(*proto.RuntimeRemoteObject); ok { // remote object
			formated = append(formated, &proto.RuntimeCallArgument{ObjectID: obj.ObjectID})
		} else { // plain json data
			formated = append(formated, &proto.RuntimeCallArgument{Value: gson.New(arg)})
		}
	}
	return formated, nil
}

func (p *Page) getJSHelperObj() (*proto.RuntimeRemoteObject, error) {
	if p.jsHelperObj == nil {
		helper, err := proto.RuntimeCallFunctionOn{
			ExecutionContextID:  p.getJSContextID(),
			FunctionDeclaration: assets.Helper,
		}.Call(p)
		if err != nil {
			return nil, err
		}
		p.jsHelperObj = helper.Result
	}

	return p.jsHelperObj, nil
}
