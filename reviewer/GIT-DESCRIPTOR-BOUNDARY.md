# Git worker descriptor observation

The private descriptor observer supplements process and mount checks before the
private [factory](GIT-FACTORY.md) grants work. It returns no source or reusable admission
capability. The caller retains the original worker pidfd and rechecks its
identity before and after observation.

The observer enumerates bounded kernel `fdinfo`, reads the original descriptors'
current flags, and duplicates actual objects through that original pidfd. A
duplicate always has CLOEXEC itself, so its descriptor flags cannot establish
the original CLOEXEC bit. Reading kernel `fdinfo` does. See Linux
[fdinfo](https://man7.org/linux/man-pages/man5/proc_pid_fdinfo.5.html) and
[pidfd_getfd](https://man7.org/linux/man-pages/man2/pidfd_getfd.2.html).

Inspection never reads object contents or changes their offsets, status flags,
socket options or connection state. It checks the object again through the
original pidfd and requires stable original flags, identity and descriptor
numbers. Any permission error, unknown descriptor or observed change refuses.
This is a readiness check on the measured inert worker before candidate code
runs, not a general guarantee against an adversary changing a running process.

The fixed worker has null standard streams. All private descriptors require
CLOEXEC. The exact export write pipe must match the factory's retained read
endpoint. Exactly one control connection must be an unnamed AF_UNIX SEQPACKET
client connected to the original host pathname of the factory's actual
accepted control connection, with the current supervisor's kernel peer
credentials. The namespace-local `/broker/worker.sock` alias does not change the
kernel's stored server pathname. The accepted connection's original peer pidfd
must match the retained worker. Exactly one pidfd must match the retained original supervisor.
The measured Go runtime's CLOEXEC eventpoll and eventfd handles are permitted.
Extra sockets, directories, regular files, pipes, pidfds and other anonymous
objects refuse, even when they have CLOEXEC. Socket domain checks never exercise
inherited Internet sockets.

## Validation

Static policy fixtures cover role replacement, missing or duplicate descriptors,
wrong access modes, missing CLOEXEC, extra host files/directories/sockets,
Internet domains and unknown anonymous objects. These are parser/policy tests.
The ordinary inert child test waits for its kernel sleep, then inspects actual
null descriptors without leaking handles. Waiting for sleep avoids inspecting
the dynamic loader's temporary library descriptors before inert readiness.

An explicit private-namespace fixture runs an ordinary measured Go executable,
authenticates the original supervisor and disables dumpability. It supplies a
real export pipe and tests the fixed roles plus actual extra host file,
directory, Internet socket and inheritable descriptor cases. It uses trusted
empty fixture storage, never candidate input or a production storage fallback.

```sh
SFL_GIT_DESCRIPTOR_KERNEL=1 SFL_GIT_BOOTSTRAP_NAMESPACE=1 \
  go -C reviewer test -race -count=1 -v -run '^TestGitDescriptorActualPrivateWorker$' .
```

Mini denies traversal of the nondumpable worker's `/proc/PID/fd` directory.
The implementation enumerates `fdinfo` and uses original-pidfd duplication;
it does not relax dumpability, change host policy or substitute pathname opens
for retained kernel object identities. The explicit fixture's actual result
must accompany any qualification claim. The ordinary suite skips this opt-in.

This does not qualify sealed worker execution, the required whole-unit noswap
tmpfs, installed-filter provenance, unit/cgroup ownership, authenticated export
publication or controller recovery. Those remain separate factory requirements
in #176. No provider credentials, model inference or live activation occur here.
