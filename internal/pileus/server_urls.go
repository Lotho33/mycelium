package pileus

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"mycelium/internal/core"
)

// Server-relative URLs (core.ServerURLPrefix) in plugin data become absolute
// on the way out, with the base the calling client reaches this server at
// (clientProxyBase: the same address /proxy URLs use). Responses can be
// shared with caches, so a message holding one is cloned before rewriting;
// messages without any go out untouched, at the cost of one walk.

func serverURLUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err == nil {
		if m, ok := resp.(proto.Message); ok {
			resp = absolutizeServerURLs(ctx, m)
		}
	}
	return resp, err
}

func serverURLStream(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(srv, &serverURLServerStream{ServerStream: ss})
}

type serverURLServerStream struct{ grpc.ServerStream }

func (s *serverURLServerStream) SendMsg(m any) error {
	if pm, ok := m.(proto.Message); ok {
		m = absolutizeServerURLs(s.Context(), pm)
	}
	return s.ServerStream.SendMsg(m)
}

func absolutizeServerURLs(ctx context.Context, m proto.Message) proto.Message {
	if m == nil || !hasServerURL(m.ProtoReflect()) {
		return m
	}
	scheme, host := clientProxyBase(ctx)
	c := proto.Clone(m)
	rewriteServerURLs(c.ProtoReflect(), scheme+"://"+host)
	return c
}

func hasServerURL(m protoreflect.Message) bool {
	found := false
	walkStrings(m, func(s string) (string, bool) {
		if core.IsServerURL(s) {
			found = true
			return s, false // stop
		}
		return s, true
	})
	return found
}

func rewriteServerURLs(m protoreflect.Message, base string) {
	walkStrings(m, func(s string) (string, bool) {
		return core.AbsoluteServerURL(s, base), true
	})
}

// walkStrings calls fn on every string in m (fields, lists, map values,
// nested messages), storing what fn returns when it differs. fn returns
// false to stop the walk. Writes are applied after each Range: mutating a
// message or map while ranging over it is undefined.
func walkStrings(m protoreflect.Message, fn func(string) (string, bool)) bool {
	cont := true
	var sets []func()
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len() && cont; i++ {
				i := i
				cont = walkValue(fd, l.Get(i), func(nv protoreflect.Value) { l.Set(i, nv) }, fn)
			}
		case fd.IsMap():
			mp := v.Map()
			vd := fd.MapValue()
			var msets []func()
			mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				cont = walkValue(vd, mv, func(nv protoreflect.Value) { msets = append(msets, func() { mp.Set(k, nv) }) }, fn)
				return cont
			})
			for _, s := range msets {
				s()
			}
		default:
			cont = walkValue(fd, v, func(nv protoreflect.Value) { sets = append(sets, func() { m.Set(fd, nv) }) }, fn)
		}
		return cont
	})
	for _, s := range sets {
		s()
	}
	return cont
}

func walkValue(fd protoreflect.FieldDescriptor, v protoreflect.Value, set func(protoreflect.Value), fn func(string) (string, bool)) bool {
	switch fd.Kind() {
	case protoreflect.StringKind:
		s := v.String()
		ns, cont := fn(s)
		if ns != s {
			set(protoreflect.ValueOfString(ns))
		}
		return cont
	case protoreflect.MessageKind, protoreflect.GroupKind:
		return walkStrings(v.Message(), fn)
	}
	return true
}
