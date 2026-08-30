// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abi_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// TestContractGeneratedRoundtrip proves every ABI message round-trips through
// the proto wire format with full fidelity (issue #1135 test plan:
// contract_generated_roundtrip), and that unknown fields survive a decode/
// encode cycle — the forward-compatibility property the S2 freeze relies on:
// after the freeze the schema evolves additively only, so older consumers
// must be able to carry fields they do not know.
func TestContractGeneratedRoundtrip(t *testing.T) {
	files := abiFiles(t)
	checked := 0
	for _, fd := range files {
		for i := 0; i < fd.Messages().Len(); i++ {
			md := fd.Messages().Get(i)
			for _, variant := range populatedVariants(t, md) {
				roundtripOne(t, md, variant)
				unknownFieldsSurvive(t, md, variant)
				checked++
			}
		}
	}
	if checked < 30 {
		t.Fatalf("roundtrip exercised only %d message variants — descriptors not fully enumerated", checked)
	}
}

func roundtripOne(t *testing.T, md protoreflect.MessageDescriptor, src protoreflect.Message) {
	t.Helper()
	wire, err := proto.Marshal(src.Interface())
	if err != nil {
		t.Fatalf("%s: marshal: %v", md.FullName(), err)
	}
	dst := src.Type().New()
	if err := proto.Unmarshal(wire, dst.Interface()); err != nil {
		t.Fatalf("%s: unmarshal: %v", md.FullName(), err)
	}
	if !proto.Equal(src.Interface(), dst.Interface()) {
		t.Errorf("%s: roundtrip mismatch\nsrc: %v\ndst: %v", md.FullName(), src, dst)
	}
}

const unknownFieldNumber = protowire.Number(9999)

func unknownFieldsSurvive(t *testing.T, md protoreflect.MessageDescriptor, src protoreflect.Message) {
	t.Helper()
	wire, err := proto.Marshal(src.Interface())
	if err != nil {
		t.Fatalf("%s: marshal: %v", md.FullName(), err)
	}
	wire = protowire.AppendTag(wire, unknownFieldNumber, protowire.VarintType)
	wire = protowire.AppendVarint(wire, 42)

	dst := src.Type().New()
	if err := proto.Unmarshal(wire, dst.Interface()); err != nil {
		t.Fatalf("%s: unmarshal with unknown field: %v", md.FullName(), err)
	}
	rewire, err := proto.Marshal(dst.Interface())
	if err != nil {
		t.Fatalf("%s: re-marshal: %v", md.FullName(), err)
	}
	found := false
	for len(rewire) > 0 {
		num, _, n := protowire.ConsumeField(rewire)
		if n <= 0 {
			t.Fatalf("%s: corrupt wire bytes during scan", md.FullName())
		}
		if num == unknownFieldNumber {
			found = true
		}
		rewire = rewire[n:]
	}
	if !found {
		t.Errorf("%s: unknown field %d lost across decode/encode — forward compatibility broken", md.FullName(), unknownFieldNumber)
	}
}

// populatedVariants returns one fully-populated message per oneof member plus
// the base (all non-oneof fields set), so oneof arms are exercised
// individually.
func populatedVariants(t *testing.T, md protoreflect.MessageDescriptor) []protoreflect.Message {
	t.Helper()
	mt := messageType(t, md)
	root := mt.New()
	populate(t, root, 0)

	variants := []protoreflect.Message{root}
	for i := 0; i < md.Oneofs().Len(); i++ {
		od := md.Oneofs().Get(i)
		if od.IsSynthetic() {
			continue
		}
		for j := 0; j < od.Fields().Len(); j++ {
			v := mt.New()
			populate(t, v, 0)
			setField(t, v, od.Fields().Get(j), 0)
			variants = append(variants, v)
		}
	}
	return variants
}

func messageType(t *testing.T, md protoreflect.MessageDescriptor) protoreflect.MessageType {
	t.Helper()
	mt, err := protoregistry.GlobalTypes.FindMessageByName(md.FullName())
	if err != nil {
		t.Fatalf("message %s not registered in the global type registry: %v", md.FullName(), err)
	}
	return mt
}

func populate(t *testing.T, m protoreflect.Message, depth int) {
	t.Helper()
	if depth > 3 {
		return
	}
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if fd.ContainingOneof() != nil && !fd.ContainingOneof().IsSynthetic() {
			continue
		}
		setField(t, m, fd, depth)
	}
}

func setField(t *testing.T, m protoreflect.Message, fd protoreflect.FieldDescriptor, depth int) {
	t.Helper()
	switch {
	case fd.IsMap():
		mv := m.NewField(fd)
		for i := 0; i < 2; i++ {
			key := protoreflect.ValueOfString(string(rune('a' + i))).MapKey()
			mv.Map().Set(key, singularValue(t, fd.MapValue(), depth))
		}
		m.Set(fd, mv)
	case fd.IsList():
		l := m.NewField(fd)
		for i := 0; i < 2; i++ {
			l.List().Append(singularValue(t, fd, depth))
		}
		m.Set(fd, l)
	default:
		m.Set(fd, singularValue(t, fd, depth))
	}
}

func singularValue(t *testing.T, fd protoreflect.FieldDescriptor, depth int) protoreflect.Value {
	t.Helper()
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(7)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(7)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(7)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(7)
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(1.5)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(1.5)
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(string(fd.JSONName()))
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte(`{"kind":"probe"}`))
	case protoreflect.EnumKind:
		ed := fd.Enum()
		for i := 0; i < ed.Values().Len(); i++ {
			if ed.Values().Get(i).Number() != 0 {
				return protoreflect.ValueOfEnum(ed.Values().Get(i).Number())
			}
		}
		return protoreflect.ValueOfEnum(ed.Values().Get(0).Number())
	case protoreflect.MessageKind:
		sub := messageType(t, fd.Message()).New()
		populate(t, sub, depth+1)
		return protoreflect.ValueOfMessage(sub)
	default:
		t.Fatalf("unhandled field kind %v for %s", fd.Kind(), fd.FullName())
		return protoreflect.Value{}
	}
}
