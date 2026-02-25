//! Direct eBPF map access via raw BPF syscalls.
//!
//! Opens a pinned BPF map (redirect_proxy_map) and performs lookup+delete
//! to resolve original destinations without any IPC round-trip to Go.
//!
//! Map layout (from k_maps.h / k_structs.h):
//!   Key:   u16 (source port)
//!   Value: dest_info_t {
//!       ip_version: u32,
//!       dest_ip4:   u32,
//!       dest_ip6:   [u32; 4],
//!       dest_port:  u32,
//!       kernelPid:  u32,
//!   }  // total: 32 bytes

use std::ffi::CString;
use std::os::unix::io::RawFd;
use tracing::{debug, error};

/// Matches the kernel's `struct dest_info_t` exactly (32 bytes).
#[repr(C)]
#[derive(Debug, Clone, Default)]
pub struct DestInfo {
    pub ip_version: u32,
    pub dest_ip4: u32,
    pub dest_ip6: [u32; 4],
    pub dest_port: u32,
    pub kernel_pid: u32,
}

/// BPF command constants (from linux/bpf.h)
const BPF_OBJ_GET: libc::c_int = 7;
const BPF_MAP_LOOKUP_ELEM: libc::c_int = 1;
const BPF_MAP_DELETE_ELEM: libc::c_int = 3;

/// Attribute union for bpf() syscall — we only use the fields we need.
/// The kernel's `union bpf_attr` is a 128-byte union; we zero-init and set
/// the relevant fields for each operation.
#[repr(C)]
union BpfAttr {
    obj_get: BpfAttrObjGet,
    map_elem: BpfAttrMapElem,
    _pad: [u8; 128],
}

#[repr(C)]
#[derive(Copy, Clone)]
struct BpfAttrObjGet {
    pathname: u64, // pointer to pinned path
    bpf_fd: u32,
    file_flags: u32,
}

#[repr(C)]
#[derive(Copy, Clone)]
struct BpfAttrMapElem {
    map_fd: u32,
    key: u64,   // pointer to key
    value: u64, // pointer to value (or next_key)
    flags: u64,
}

fn bpf_syscall(cmd: libc::c_int, attr: &mut BpfAttr, size: usize) -> libc::c_long {
    unsafe { libc::syscall(libc::SYS_bpf, cmd, attr as *mut BpfAttr, size) }
}

/// Handle wrapping a BPF map file descriptor, with RAII cleanup.
pub struct BpfMapHandle {
    fd: RawFd,
}

impl BpfMapHandle {
    /// Open a pinned BPF map by its filesystem path (e.g. `/sys/fs/bpf/keploy_redirect_proxy_map`).
    pub fn open_pinned(path: &str) -> std::io::Result<Self> {
        let c_path = CString::new(path).map_err(|e| {
            std::io::Error::new(std::io::ErrorKind::InvalidInput, e)
        })?;

        let mut attr = BpfAttr {
            _pad: [0u8; 128],
        };
        attr.obj_get = BpfAttrObjGet {
            pathname: c_path.as_ptr() as u64,
            bpf_fd: 0,
            file_flags: 0,
        };

        let fd = bpf_syscall(BPF_OBJ_GET, &mut attr, std::mem::size_of::<BpfAttrObjGet>());
        if fd < 0 {
            let err = std::io::Error::last_os_error();
            error!("BPF_OBJ_GET({}) failed: {}", path, err);
            return Err(err);
        }

        debug!("Opened pinned BPF map {} -> fd {}", path, fd);
        Ok(Self { fd: fd as RawFd })
    }

    /// Lookup a destination by source port. Returns None if the key doesn't exist.
    pub fn lookup(&self, src_port: u16) -> Option<DestInfo> {
        let key = src_port;
        let mut value = DestInfo::default();

        let mut attr = BpfAttr {
            _pad: [0u8; 128],
        };
        attr.map_elem = BpfAttrMapElem {
            map_fd: self.fd as u32,
            key: &key as *const u16 as u64,
            value: &mut value as *mut DestInfo as u64,
            flags: 0,
        };

        let ret = bpf_syscall(
            BPF_MAP_LOOKUP_ELEM,
            &mut attr,
            std::mem::size_of::<BpfAttrMapElem>(),
        );
        if ret < 0 {
            // ENOENT is expected for unknown ports
            return None;
        }

        Some(value)
    }

    /// Delete a source port entry from the map.
    pub fn delete(&self, src_port: u16) {
        let key = src_port;

        let mut attr = BpfAttr {
            _pad: [0u8; 128],
        };
        attr.map_elem = BpfAttrMapElem {
            map_fd: self.fd as u32,
            key: &key as *const u16 as u64,
            value: 0,
            flags: 0,
        };

        let ret = bpf_syscall(
            BPF_MAP_DELETE_ELEM,
            &mut attr,
            std::mem::size_of::<BpfAttrMapElem>(),
        );
        if ret < 0 {
            debug!("BPF_MAP_DELETE_ELEM for port {} returned error (may already be deleted)", src_port);
        }
    }

    /// Lookup and delete in one go (atomic from the caller's perspective).
    pub fn lookup_and_delete(&self, src_port: u16) -> Option<DestInfo> {
        let info = self.lookup(src_port)?;
        self.delete(src_port);
        Some(info)
    }
}

impl Drop for BpfMapHandle {
    fn drop(&mut self) {
        unsafe {
            libc::close(self.fd);
        }
    }
}

/// Format a DestInfo into an IP string + port tuple that the proxy can use to dial.
pub fn format_dest(info: &DestInfo) -> (String, u16) {
    let ip = if info.ip_version == 4 {
        format!(
            "{}.{}.{}.{}",
            (info.dest_ip4 >> 24) & 0xFF,
            (info.dest_ip4 >> 16) & 0xFF,
            (info.dest_ip4 >> 8) & 0xFF,
            info.dest_ip4 & 0xFF,
        )
    } else {
        // IPv6: simplified — convert 4 x u32 to hex
        let b = info.dest_ip6;
        format!(
            "{:x}:{:x}:{:x}:{:x}:{:x}:{:x}:{:x}:{:x}",
            (b[0] >> 16) & 0xFFFF, b[0] & 0xFFFF,
            (b[1] >> 16) & 0xFFFF, b[1] & 0xFFFF,
            (b[2] >> 16) & 0xFFFF, b[2] & 0xFFFF,
            (b[3] >> 16) & 0xFFFF, b[3] & 0xFFFF,
        )
    };
    (ip, info.dest_port as u16)
}
