#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <linux/memfd.h>
#include <sched.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/prctl.h>
#include <sys/reboot.h>
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/syscall.h>
#include <sys/utsname.h>
#include <sys/wait.h>
#include <unistd.h>

#ifndef MFD_EXEC
#define MFD_EXEC 0x0010U
#endif

static int comma_token(const char *options, const char *wanted) {
    size_t length = strlen(wanted);
    for (const char *at = options; *at;) {
        const char *end = strchr(at, ',');
        size_t size = end ? (size_t)(end-at) : strlen(at);
        if (size == length && !memcmp(at, wanted, length)) return 1;
        if (!end) break;
        at = end+1;
    }
    return 0;
}

static int work_mount_noswap(const char *row) {
    char copy[8192], *left_state = NULL, *right_state = NULL;
    if (strlen(row) >= sizeof(copy)) return 0;
    strcpy(copy, row);
    char *separator = strstr(copy, " - ");
    if (!separator) return 0;
    *separator = 0;
    char *field = strtok_r(copy, " ", &left_state);
    for (int index = 0; field && index < 4; index++) field = strtok_r(NULL, " ", &left_state);
    if (!field || strcmp(field, "/work")) return 0;
    char *type = strtok_r(separator+3, " \r\n", &right_state);
    char *source = strtok_r(NULL, " \r\n", &right_state);
    char *options = strtok_r(NULL, " \r\n", &right_state);
    return type && source && options && !strcmp(type, "tmpfs") &&
        !strtok_r(NULL, " \r\n", &right_state) && comma_token(options, "noswap");
}

static int zero_capabilities(void) {
    FILE *f = fopen("/proc/self/status", "r");
    char line[256]; int effective = 0, bounding = 0;
    if (!f) return 0;
    while (fgets(line, sizeof(line), f)) {
        unsigned long long value;
        if (sscanf(line, "CapEff: %llx", &value) == 1) effective = value == 0;
        if (sscanf(line, "CapBnd: %llx", &value) == 1) bounding = value == 0;
    }
    fclose(f); return effective && bounding;
}

static int exec_probe(int image, const char *method) {
    int pipes[2], status; char output[128] = {0};
    if (pipe2(pipes, O_CLOEXEC)) return 0;
    pid_t outer = fork();
    if (outer == 0) {
        close(pipes[0]);
        if (dup2(pipes[1], STDOUT_FILENO) < 0) _exit(31);
        close(pipes[1]);
        if (unshare(CLONE_NEWNS | CLONE_NEWNET | CLONE_NEWIPC | CLONE_NEWUTS | CLONE_NEWPID)) _exit(32);
        if (mount(NULL, "/", NULL, MS_REC | MS_PRIVATE, NULL)) _exit(33);
        pid_t inner = fork();
        if (inner < 0) _exit(34);
        if (inner) {
            if (waitpid(inner, &status, 0) != inner) _exit(35);
            _exit(WIFEXITED(status) ? WEXITSTATUS(status) : 36);
        }
        if (mount("proc", "/proc", "proc", MS_NOSUID | MS_NODEV | MS_NOEXEC, NULL)) _exit(37);
        for (int cap = 0; cap <= 40; cap++) {
            if (prctl(PR_CAPBSET_DROP, cap, 0, 0, 0) && errno != EINVAL) _exit(38);
        }
        if (prctl(PR_CAP_AMBIENT, PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0) ||
            prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) || setgroups(0, NULL) ||
            setresgid(1203, 1203, 1203) || setresuid(1203, 1203, 1203) ||
            prctl(PR_SET_DUMPABLE, 0, 0, 0, 0)) _exit(39);
        char *arguments[] = {"sealed-probe", "--sealed-payload", NULL};
        char *environment[] = {"PATH=/nonexistent", NULL};
        if (!strcmp(method, "execveat")) {
            syscall(SYS_execveat, image, "", arguments, environment, AT_EMPTY_PATH);
        } else {
            char name[64]; snprintf(name, sizeof(name), "/proc/self/fd/%d", image);
            execve(name, arguments, environment);
        }
        _exit(errno ? errno : 40);
    }
    close(pipes[1]);
    if (outer < 0) { close(pipes[0]); return 0; }
    ssize_t count = read(pipes[0], output, sizeof(output)-1);
    close(pipes[0]);
    if (waitpid(outer, &status, 0) != outer) return 0;
    fprintf(stderr, "probe method=%s status=%d output=%s\n", method, status, output);
    return count == 17 && !memcmp(output, "SEALED-PAYLOAD-OK\n", 17) &&
        WIFEXITED(status) && WEXITSTATUS(status) == 0;
}

int main(int argc, char **argv) {
    if (argc == 2 && !strcmp(argv[1], "--sealed-payload")) {
        if (getpid() != 1 || getuid() != 1203 || geteuid() != 1203 ||
            prctl(PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0) != 1 || !zero_capabilities()) return 41;
        if (prctl(PR_SET_DUMPABLE, 0, 0, 0, 0)) return 42;
        int writable = open("/work/prerequisite-write", O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC | O_NOFOLLOW, 0600);
        if (writable < 0 || write(writable, "x", 1) != 1 || fsync(writable) || close(writable) ||
            unlink("/work/prerequisite-write")) return 44;
        return write(STDOUT_FILENO, "SEALED-PAYLOAD-OK\n", 17) == 17 ? 0 : 43;
    }
    /* Privileged calls are reachable only in this fresh credential-free VM. */
    if (argc != 2 || strcmp(argv[1], "--guest-init") || getpid() != 1 || geteuid() != 0) return 2;
    alarm(45);
    mkdir("/proc", 0555); mkdir("/work", 0700);
    if (mount("proc", "/proc", "proc", MS_NOSUID | MS_NODEV | MS_NOEXEC, NULL)) return 3;
    char cmdline[4096] = {0}, nonce[33] = {0};
    int command = open("/proc/cmdline", O_RDONLY | O_CLOEXEC);
    if (command < 0 || read(command, cmdline, sizeof(cmdline)-1) <= 0) return 4;
    close(command);
    char *start = strstr(cmdline, "sfl-fixture=");
    if (!start) return 5;
    start += strlen("sfl-fixture=");
    for (int i = 0; i < 32; i++) {
        if (!((start[i] >= '0' && start[i] <= '9') || (start[i] >= 'a' && start[i] <= 'f'))) return 6;
        nonce[i] = start[i];
    }
    int mounted = mount("tmpfs", "/work", "tmpfs", MS_NOSUID | MS_NODEV | MS_NOEXEC,
        "size=268435456,nr_inodes=65536,noswap,mode=0700") == 0;
    int mount_errno = mounted ? 0 : errno;
    if (mounted && chown("/work", 1203, 1203)) return 13;
    struct statfs fs = {0}; int fs_ok = mounted && statfs("/work", &fs) == 0;
    struct stat work = {0}; int metadata_ok = mounted && lstat("/work", &work) == 0 && S_ISDIR(work.st_mode);
    int noswap_reported = 0; char mount_line[8192];
    FILE *mounts = fopen("/proc/self/mountinfo", "r");
    if (mounts) {
        while (fgets(mount_line, sizeof(mount_line), mounts))
            if (work_mount_noswap(mount_line)) noswap_reported = 1;
        fclose(mounts);
    }
    int image = syscall(SYS_memfd_create, "sfl-git-vm-probe", MFD_CLOEXEC | MFD_ALLOW_SEALING | MFD_EXEC);
    int input = open("/init", O_RDONLY | O_CLOEXEC); char data[8192]; ssize_t n; size_t total = 0;
    if (image < 0 || input < 0) return 7;
    while ((n = read(input, data, sizeof(data))) > 0) {
        total += n;
        if (total > 4*1024*1024 || write(image, data, n) != n) return 8;
    }
    close(input);
    if (n < 0 || fchown(image, 1203, 1203) || fchmod(image, 0500) ||
        fcntl(image, F_ADD_SEALS, 63) || fcntl(image, F_GET_SEALS) != 63) return 9;
    int proc_exec = exec_probe(image, "proc"), direct_exec = exec_probe(image, "execveat");
    close(image);
    struct utsname name; if (uname(&name)) return 10;
    for (char *c = name.release; *c; c++) if (!((*c >= '0' && *c <= '9') || (*c >= 'a' && *c <= 'z') || *c == '.' || *c == '-' || *c == '+')) return 11;
    printf("SFL_GIT_VM_PREREQ_%s {\"version\":2,\"nonce\":\"%s\",\"kernel\":\"%s\",\"mount_succeeded\":%s,\"mount_errno\":%d,\"filesystem_type\":%ld,\"size_bytes\":%llu,\"inode_limit\":%llu,\"mount_flags\":%lu,\"noswap_reported\":%s,\"work_mode\":%u,\"work_uid\":%u,\"work_gid\":%u,\"work_writable\":%s,\"sealed_proc_exec\":%s,\"sealed_execveat\":%s,\"fake_only\":true,\"production_qualified\":false}\n",
        nonce, nonce, name.release, mounted ? "true" : "false", mount_errno, fs_ok ? fs.f_type : 0,
        fs_ok ? (unsigned long long)fs.f_blocks*fs.f_bsize : 0,
        fs_ok ? (unsigned long long)fs.f_files : 0, fs_ok ? fs.f_flags : 0,
        noswap_reported ? "true" : "false",
        metadata_ok ? work.st_mode & 07777 : 0, metadata_ok ? work.st_uid : 0, metadata_ok ? work.st_gid : 0,
        proc_exec && direct_exec ? "true" : "false",
        proc_exec ? "true" : "false", direct_exec ? "true" : "false");
    fflush(NULL); sync(); reboot(RB_POWER_OFF); return 12;
}
