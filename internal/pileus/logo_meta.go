package pileus

import (
	"context"

	gen "github.com/Lotho33/stipes-sdk/sdk/gen"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"mycelium/internal/logometa"
)

// Every message with a logo_url next to a logo_meta field (CatalogItem,
// MovieDetails, SeriesDetails, MyListItem) gets logo_meta filled from the
// core's measurements on the way out, so plugins don't analyse logos
// themselves. A logo not measured yet is queued and the field stays unset
// in this response. Responses can be shared with caches: a message is cloned
// before it is changed, and only when there is something to add.

var logoMetaName = (&gen.LogoMeta{}).ProtoReflect().Descriptor().FullName()

func logoMetaUnary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	resp, err := handler(ctx, req)
	if err == nil {
		if m, ok := resp.(proto.Message); ok {
			resp = fillLogoMeta(m)
		}
	}
	return resp, err
}

func fillLogoMeta(m proto.Message) proto.Message {
	if m == nil {
		return m
	}
	found := false
	walkLogos(m.ProtoReflect(), func(msg protoreflect.Message, metaFD protoreflect.FieldDescriptor, url string) {
		if _, ok := logometa.Lookup(url); ok {
			found = true
		}
	})
	if !found {
		return m
	}
	c := proto.Clone(m)
	walkLogos(c.ProtoReflect(), func(msg protoreflect.Message, metaFD protoreflect.FieldDescriptor, url string) {
		if lm, ok := logometa.Lookup(url); ok {
			msg.Set(metaFD, protoreflect.ValueOfMessage(toProto(lm).ProtoReflect()))
		}
	})
	return c
}

func toProto(m logometa.Meta) *gen.LogoMeta {
	return &gen.LogoMeta{
		Width:      int32(m.W),
		Height:     int32(m.H),
		LumLow:     m.Low,
		LumMid:     m.Mid,
		LumHigh:    m.High,
		DarkShare:  m.DarkShare,
		LightShare: m.LightShare,
		Coverage:   m.Coverage,
		Color:      m.Color,
		Halo:       m.Halo,
	}
}

// walkLogos calls fn for every message in m (m included) that has a
// non-empty logo_url and an unset logo_meta field.
func walkLogos(m protoreflect.Message, fn func(msg protoreflect.Message, metaFD protoreflect.FieldDescriptor, url string)) {
	fields := m.Descriptor().Fields()
	if urlFD, metaFD := fields.ByName("logo_url"), fields.ByName("logo_meta"); urlFD != nil && metaFD != nil &&
		urlFD.Kind() == protoreflect.StringKind && metaFD.Message() != nil && metaFD.Message().FullName() == logoMetaName {
		if u := m.Get(urlFD).String(); u != "" && !m.Has(metaFD) {
			fn(m, metaFD, u)
		}
	}
	// Only children are changed while ranging over m, never m itself.
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsMap():
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					walkLogos(mv.Message(), fn)
					return true
				})
			}
		case fd.Kind() != protoreflect.MessageKind && fd.Kind() != protoreflect.GroupKind:
		case fd.Message().FullName() == logoMetaName:
		case fd.IsList():
			l := v.List()
			for i := 0; i < l.Len(); i++ {
				walkLogos(l.Get(i).Message(), fn)
			}
		default:
			walkLogos(v.Message(), fn)
		}
		return true
	})
}
