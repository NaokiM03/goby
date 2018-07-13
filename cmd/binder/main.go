package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/ioutil"
	"log"
	"strings"

	"github.com/fatih/camelcase"

	// makes writing this easier
	. "github.com/dave/jennifer/jen"
)

var (
	in       = flag.String("in", ".", "folder to create bindings from")
	typeName = flag.String("type", "", "type to generate bindings for")
)

const (
	vmPkg     = "github.com/goby-lang/goby/vm"
	errorsPkg = "github.com/goby-lang/goby/vm/errors"
)

func typeFromExpr(e ast.Expr) string {
	var name string
	switch t := e.(type) {
	case *ast.Ident:
		name = t.Name

	case *ast.StarExpr:
		name = fmt.Sprintf("*%s", typeFromExpr(t.X))

	case *ast.SelectorExpr:
		name = fmt.Sprintf("%s.%s", typeFromExpr(t.X), t.Sel.Name)

	}
	return name
}

func typeNameFromExpr(e ast.Expr) string {
	var name string
	switch t := e.(type) {
	case *ast.Ident:
		name = t.Name

	case *ast.StarExpr:
		name = typeFromExpr(t.X)

	case *ast.SelectorExpr:
		name = fmt.Sprintf("%s.%s", typeFromExpr(t.X), t.Sel.Name)

	}
	return name
}

type argPair struct {
	name, kind string
}

func allArgs(f *ast.FieldList) []argPair {
	var args []argPair
	for _, l := range f.List {
		for _, n := range l.Names {
			args = append(args, argPair{
				name: n.Name,
				kind: typeNameFromExpr(l.Type),
			})
		}
	}

	return args
}

// Binding holds context about a struct that represents a goby class.
type Binding struct {
	ClassName       string
	ClassMethods    []*ast.FuncDecl // Any method defined without a pointer reciever is a class method func (Class) myFunc
	InstanceMethods []*ast.FuncDecl // Any method defined with a pointer reciever is an instance method func (c *Class) myFunc

}

func (b *Binding) staticName() string {
	return fmt.Sprintf("static%s", b.ClassName)
}

func (b *Binding) bindingName(f *ast.FuncDecl) string {
	return fmt.Sprintf("binding%s%s", b.ClassName, f.Name.Name)
}

// BindMethods generates code that binds methods of a go structure to a goby class
func (b *Binding) BindMethods(f *File, x *ast.File) {
	f.Add(mapping(b, x.Name.Name))
	f.Var().Id(b.staticName()).Op("=").New(Id(b.ClassName))
	for _, c := range b.ClassMethods {
		b.BindClassMethod(f, c)
		f.Line()
	}
	for _, c := range b.InstanceMethods {
		b.BindInstanceMethod(f, c)
		f.Line()
	}
}

// BindClassMethod will generate class method bindings.
// This is a global static method associated with the class.
func (b *Binding) BindClassMethod(f *File, d *ast.FuncDecl) {
	r := Id("r").Op(":=").Id(b.staticName()).Line()
	b.body(r, f, d)
}

// BindInstanceMethod will generate instance method bindings.
// This function will be bound to a spesific instantation of a goby class.
func (b *Binding) BindInstanceMethod(f *File, d *ast.FuncDecl) {
	r := List(Id("r"), Id("ok")).Op(":=").Add(Id("receiver")).Assert(Op("*").Id(b.ClassName)).Line()
	r = r.If(Op("!").Id("ok")).Block(
		Panic(
			Qual("fmt", "Sprintf").Call(Lit("Impossible receiver type. Wanted "+b.ClassName+" got %s"), Id("receiver")),
		),
	).Line()
	b.body(r, f, d)
}

func wrongArgNum(want int) Code {
	return Return(Id("t").Dot("VM").Call().Dot("InitErrorObject").Call(
		Qual(errorsPkg, "ArgumentError"),
		Id("line"),
		Qual(errorsPkg, "WrongNumberOfArgumentFormat"),
		Lit(want),
		Id("len").Call(Id("args")),
	))
}

func wrongArgType(name, want string) Code {
	return Return(Id("t").Dot("VM").Call().Dot("InitErrorObject").Call(
		Qual(errorsPkg, "TypeError"),
		Id("line"),
		Qual(errorsPkg, "WrongArgumentTypeFormat"),
		Lit(want),
		Id(name).Dot("Class").Call().Dot("Name"),
	))
}

// body is a helper function for generating the common body of a method
func (b *Binding) body(receiver *Statement, f *File, d *ast.FuncDecl) {
	s := f.Func().Id(b.bindingName(d))
	s = s.Params(
		Id("receiver").Qual(vmPkg, "Object"),
		Id("line").Id("int"),
		Id("t").Op("*").Qual(vmPkg, "Thread"),
		Id("args").Index().Qual(vmPkg, "Object"),
	).Qual(vmPkg, "Object")

	var args []*Statement
	for i, a := range allArgs(d.Type.Params) {
		if i == 0 {
			continue
		}
		i = i - 1
		c := List(Id(fmt.Sprintf("arg%d", i)), Id("ok")).Op(":=").Id("args").Index(Lit(i)).Assert(Id(a.kind))
		c = c.Line()
		c = c.If(Op("!").Id("ok")).Block(
			Panic(Lit(fmt.Sprintf("Argument %d must be %s", i, a.kind))),
		).Line()
		args = append(args, c)
	}

	inner := receiver.If(Len(Id("args")).Op("!=").Lit(d.Type.Params.NumFields() - 1)).Block(
		wrongArgNum(d.Type.Params.NumFields() - 1),
	).Line()
	argNames := []Code{
		Id("t"),
	}
	for i, a := range args {
		inner = inner.Add(a).Line()
		argNames = append(argNames, Id(fmt.Sprintf("arg%d", i)))
	}

	inner = inner.Return(Id("r").Dot(d.Name.Name).Call(argNames...))
	s.Block(inner)
}

// mapping generates the "init" portion of the bindings.
// This will call hooks in the vm package to load the class definition at runtime.
func mapping(b *Binding, pkg string) Code {
	fnName := func(s string) string {
		x := camelcase.Split(s)
		return strings.ToLower(strings.Join(x, "_"))
	}

	cm := Dict{}
	for _, d := range b.ClassMethods {
		cm[Lit(fnName(d.Name.Name))] = Id(b.bindingName(d))
	}
	im := Dict{}
	for _, d := range b.InstanceMethods {
		im[Lit(fnName(d.Name.Name))] = Id(b.bindingName(d))
	}
	dm := Qual(vmPkg, "RegisterExternalClass").Call(
		Line().Lit(pkg),
		Qual(vmPkg, "ExternalClass").Call(
			Line().Lit(b.ClassName),
			Line().Lit(pkg+".gb"),
			Line().Map(String()).Qual(vmPkg, "Method").Values(cm),
			Line().Map(String()).Qual(vmPkg, "Method").Values(im),
		),
	)
	l := Func().Id("init").Params().Block(
		dm,
	)
	return l
}

func main() {
	flag.Parse()

	fs := token.NewFileSet()
	buff, err := ioutil.ReadFile(*in)
	if err != nil {
		log.Fatal(err)
	}

	f, err := parser.ParseFile(fs, *in, string(buff), parser.AllErrors)
	if err != nil {
		log.Fatal(err)
	}

	bindings := make(map[string]*Binding)

	// iterate though every node in the ast looking for function definitions
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncDecl:
			if n.Recv != nil {
				res := n.Type.Results
				if res == nil {
					return true
				}

				if len(res.List) == 0 || typeNameFromExpr(res.List[0].Type) != "Object" {
					return true
				}

				// class or instance?
				r := n.Recv.List[0]
				name := typeNameFromExpr(r.Type)

				b, ok := bindings[name]
				if !ok {
					b = new(Binding)
					b.ClassName = name
					bindings[name] = b
				}

				// class
				if r.Names == nil {
					b.ClassMethods = append(b.ClassMethods, n)
				} else {
					b.InstanceMethods = append(b.InstanceMethods, n)
				}
			}
		case *ast.TypeSpec:
			bindings[n.Name.Name] = &Binding{
				ClassName: n.Name.Name,
			}

		}

		return true
	})

	bnd, ok := bindings[*typeName]
	if !ok {
		log.Fatal("Uknown type", *typeName)
	}

	o := NewFile(f.Name.Name)
	bnd.BindMethods(o, f)

	err = o.Save("bindings.go")
	if err != nil {
		log.Fatal(err)
	}
}
