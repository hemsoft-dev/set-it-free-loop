#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/syscall.h>
#include <unistd.h>
static void quoted(const char *s){putchar('"');for(;*s;s++){unsigned char c=*s;if(c=='"'||c=='\\'){putchar('\\');putchar(c);}else if(c<32){printf("\\u%04x",c);}else putchar(c);}putchar('"');}
static void file_field(const char *key,const char *path){char b[256]={0};int fd=open(path,O_RDONLY|O_CLOEXEC);ssize_t n=fd<0?-1:read(fd,b,sizeof(b)-1);if(fd>=0)close(fd);if(n<0)snprintf(b,sizeof(b),"unavailable:%d",errno);else b[n]=0;b[strcspn(b,"\n")]=0;printf(",");quoted(key);printf(":");quoted(b);}
int main(int argc,char **argv){
 if(argc==2&&!strcmp(argv[1],"--target")){printf("{\"event\":\"target\",\"ok\":true,\"uid\":%d}\n",getuid());return 0;}
 if(argc!=3||(strcmp(argv[1],"proc")&&strcmp(argv[1],"direct")&&strcmp(argv[1],"named")))return 2;
 int fd=atoi(argv[2]);struct stat s;struct statfs fs;uid_t r,e,saved;gid_t gr,ge,gs;
 if(fd<3||fd>255||fstat(fd,&s)||fstatfs(fd,&fs)||getresuid(&r,&e,&saved)||getresgid(&gr,&ge,&gs))return 3;
 printf("{\"event\":\"before\",\"method\":");quoted(argv[1]);printf(",\"uid\":[%d,%d,%d],\"gid\":[%d,%d,%d],\"image_uid\":%d,\"image_gid\":%d,\"image_mode\":%u,\"image_bytes\":%lld,\"image_inode\":%llu,\"image_seals\":%d,\"mount_flags\":%lu,\"dumpable\":%d,\"no_new_privs\":%d",r,e,saved,gr,ge,gs,s.st_uid,s.st_gid,s.st_mode&0777,(long long)s.st_size,(unsigned long long)s.st_ino,fcntl(fd,F_GET_SEALS),(unsigned long)fs.f_flags,prctl(PR_GET_DUMPABLE),prctl(PR_GET_NO_NEW_PRIVS,0,0,0,0));
 file_field("profile","/proc/self/attr/current");file_field("uid_map","/proc/self/uid_map");file_field("gid_map","/proc/self/gid_map");printf("}\n");fflush(stdout);
 char *args[]={"inert-sealed-target","--target",NULL};char *env[]={NULL};
 if(!strcmp(argv[1],"named")){struct stat named;struct statfs namedfs;int n=open("/bound-worker",O_RDONLY|O_CLOEXEC);if(n<0||fstat(n,&named)||fstatfs(n,&namedfs)||named.st_ino!=s.st_ino||named.st_dev!=s.st_dev)return 5;printf("{\"event\":\"named_inode\",\"same_inode\":true,\"seals\":%d,\"mount_flags\":%lu}\n",fcntl(n,F_GET_SEALS),(unsigned long)namedfs.f_flags);fflush(stdout);close(n);execve("/bound-worker",args,env);}
 else if(!strcmp(argv[1],"direct"))syscall(SYS_execveat,fd,"",args,env,AT_EMPTY_PATH);
 else{char p[64];snprintf(p,sizeof(p),"/proc/self/fd/%d",fd);execve(p,args,env);}
 int failure=errno;printf("{\"event\":\"exec_failed\",\"errno\":%d}\n",failure);return failure?failure:4;
}
