// Code generated by protoc-gen-go. DO NOT EDIT.
// source: vm.proto

package deprecatedzconfig

import proto "github.com/golang/protobuf/proto"
import fmt "fmt"
import math "math"

// Reference imports to suppress errors if they are not otherwise used.
var _ = proto.Marshal
var _ = fmt.Errorf
var _ = math.Inf

type VmConfig struct {
	Kernel     string   `protobuf:"bytes,1,opt,name=kernel" json:"kernel,omitempty"`
	Ramdisk    string   `protobuf:"bytes,2,opt,name=ramdisk" json:"ramdisk,omitempty"`
	Memory     uint32   `protobuf:"varint,3,opt,name=memory" json:"memory,omitempty"`
	Maxmem     uint32   `protobuf:"varint,4,opt,name=maxmem" json:"maxmem,omitempty"`
	Vcpus      uint32   `protobuf:"varint,5,opt,name=vcpus" json:"vcpus,omitempty"`
	Maxcpus    uint32   `protobuf:"varint,6,opt,name=maxcpus" json:"maxcpus,omitempty"`
	Rootdev    string   `protobuf:"bytes,7,opt,name=rootdev" json:"rootdev,omitempty"`
	Extraargs  string   `protobuf:"bytes,8,opt,name=extraargs" json:"extraargs,omitempty"`
	Bootloader string   `protobuf:"bytes,9,opt,name=bootloader" json:"bootloader,omitempty"`
	Cpus       string   `protobuf:"bytes,10,opt,name=cpus" json:"cpus,omitempty"`
	Devicetree string   `protobuf:"bytes,11,opt,name=devicetree" json:"devicetree,omitempty"`
	Dtdev      []string `protobuf:"bytes,12,rep,name=dtdev" json:"dtdev,omitempty"`
	Irqs       []uint32 `protobuf:"varint,13,rep,packed,name=irqs" json:"irqs,omitempty"`
	Iomem      []string `protobuf:"bytes,14,rep,name=iomem" json:"iomem,omitempty"`
}

func (m *VmConfig) Reset()                    { *m = VmConfig{} }
func (m *VmConfig) String() string            { return proto.CompactTextString(m) }
func (*VmConfig) ProtoMessage()               {}
func (*VmConfig) Descriptor() ([]byte, []int) { return fileDescriptor5, []int{0} }

func (m *VmConfig) GetKernel() string {
	if m != nil {
		return m.Kernel
	}
	return ""
}

func (m *VmConfig) GetRamdisk() string {
	if m != nil {
		return m.Ramdisk
	}
	return ""
}

func (m *VmConfig) GetMemory() uint32 {
	if m != nil {
		return m.Memory
	}
	return 0
}

func (m *VmConfig) GetMaxmem() uint32 {
	if m != nil {
		return m.Maxmem
	}
	return 0
}

func (m *VmConfig) GetVcpus() uint32 {
	if m != nil {
		return m.Vcpus
	}
	return 0
}

func (m *VmConfig) GetMaxcpus() uint32 {
	if m != nil {
		return m.Maxcpus
	}
	return 0
}

func (m *VmConfig) GetRootdev() string {
	if m != nil {
		return m.Rootdev
	}
	return ""
}

func (m *VmConfig) GetExtraargs() string {
	if m != nil {
		return m.Extraargs
	}
	return ""
}

func (m *VmConfig) GetBootloader() string {
	if m != nil {
		return m.Bootloader
	}
	return ""
}

func (m *VmConfig) GetCpus() string {
	if m != nil {
		return m.Cpus
	}
	return ""
}

func (m *VmConfig) GetDevicetree() string {
	if m != nil {
		return m.Devicetree
	}
	return ""
}

func (m *VmConfig) GetDtdev() []string {
	if m != nil {
		return m.Dtdev
	}
	return nil
}

func (m *VmConfig) GetIrqs() []uint32 {
	if m != nil {
		return m.Irqs
	}
	return nil
}

func (m *VmConfig) GetIomem() []string {
	if m != nil {
		return m.Iomem
	}
	return nil
}

func init() {
	proto.RegisterType((*VmConfig)(nil), "VmConfig")
}

func init() { proto.RegisterFile("vm.proto", fileDescriptor5) }

var fileDescriptor5 = []byte{
	// 295 bytes of a gzipped FileDescriptorProto
	0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x02, 0xff, 0x4c, 0x91, 0xbd, 0x4e, 0xec, 0x30,
	0x10, 0x46, 0xb5, 0xff, 0x1b, 0xdf, 0x1b, 0x0a, 0x0b, 0x21, 0x17, 0x08, 0x22, 0x1a, 0x52, 0x25,
	0x05, 0x6f, 0x00, 0x1d, 0x65, 0x0a, 0x0a, 0x3a, 0xc7, 0x1e, 0x82, 0xb5, 0xf1, 0x4e, 0x70, 0x9c,
	0x28, 0xec, 0x13, 0xf3, 0x18, 0xc8, 0xe3, 0xac, 0x96, 0xce, 0xe7, 0xcc, 0x8c, 0x3f, 0x5b, 0xc3,
	0xf6, 0xa3, 0x2d, 0x3a, 0x87, 0x1e, 0x1f, 0x7e, 0x96, 0x6c, 0xff, 0x66, 0x5f, 0xf0, 0xf8, 0x61,
	0x1a, 0x7e, 0xc3, 0xb6, 0x07, 0x70, 0x47, 0x68, 0xc5, 0x22, 0x5b, 0xe4, 0x49, 0x35, 0x13, 0x17,
	0x6c, 0xe7, 0xa4, 0xd5, 0xa6, 0x3f, 0x88, 0x25, 0x15, 0xce, 0x18, 0x26, 0x2c, 0x58, 0x74, 0xdf,
	0x62, 0x95, 0x2d, 0xf2, 0xb4, 0x9a, 0x89, 0xbc, 0x9c, 0x2c, 0x58, 0xb1, 0x9e, 0x3d, 0x11, 0xbf,
	0x66, 0x9b, 0x51, 0x75, 0x43, 0x2f, 0x36, 0xa4, 0x23, 0x84, 0xfb, 0xad, 0x9c, 0xc8, 0x6f, 0xc9,
	0x9f, 0x91, 0x92, 0x11, 0xbd, 0x86, 0x51, 0xec, 0xe6, 0xe4, 0x88, 0xfc, 0x96, 0x25, 0x30, 0x79,
	0x27, 0xa5, 0x6b, 0x7a, 0xb1, 0xa7, 0xda, 0x45, 0xf0, 0x3b, 0xc6, 0x6a, 0x44, 0xdf, 0xa2, 0xd4,
	0xe0, 0x44, 0x42, 0xe5, 0x3f, 0x86, 0x73, 0xb6, 0xa6, 0x38, 0x46, 0x15, 0x3a, 0x87, 0x19, 0x0d,
	0xa3, 0x51, 0xe0, 0x1d, 0x80, 0xf8, 0x17, 0x67, 0x2e, 0x26, 0xbc, 0x5d, 0xd3, 0x4b, 0xfe, 0x67,
	0xab, 0x3c, 0xa9, 0x22, 0x84, 0x9b, 0x8c, 0xfb, 0xea, 0x45, 0x9a, 0xad, 0xf2, 0xb4, 0xa2, 0x73,
	0xe8, 0x34, 0x18, 0x3e, 0x7f, 0x15, 0x3b, 0x09, 0x9e, 0x5f, 0xd9, 0xbd, 0x42, 0x5b, 0x9c, 0x40,
	0x83, 0x96, 0x85, 0x6a, 0x71, 0xd0, 0xc5, 0xd0, 0x83, 0x0b, 0x01, 0x71, 0x1b, 0xef, 0x8f, 0x8d,
	0xf1, 0x9f, 0x43, 0x5d, 0x28, 0xb4, 0x65, 0xec, 0x2b, 0x65, 0x67, 0x4a, 0x0d, 0x9d, 0x03, 0x25,
	0x3d, 0xe8, 0x93, 0xa2, 0x4d, 0xd5, 0x5b, 0xea, 0x7f, 0xfa, 0x0d, 0x00, 0x00, 0xff, 0xff, 0xcb,
	0x5f, 0x13, 0xf9, 0xc9, 0x01, 0x00, 0x00,
}
