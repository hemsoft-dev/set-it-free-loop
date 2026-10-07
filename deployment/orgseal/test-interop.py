"""Independent libsodium verification using generated synthetic credentials only."""
import base64
import ctypes
import ctypes.util
import json
import sys

data = {key: base64.b64decode(value, validate=True) for key, value in json.load(sys.stdin).items()}
library = ctypes.CDLL(ctypes.util.find_library('sodium'))
library.sodium_init()
library.crypto_box_seal_open.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_ulonglong,
                                       ctypes.c_void_p, ctypes.c_void_p]
library.crypto_box_seal_open.restype = ctypes.c_int
plain = ctypes.create_string_buffer(len(data['message']))
assert library.crypto_box_seal_open(plain, data['ciphertext'], len(data['ciphertext']),
                                   data['public'], data['private']) == 0
assert plain.raw == data['message']
tampered = bytearray(data['ciphertext'])
tampered[-1] ^= 1
assert library.crypto_box_seal_open(plain, bytes(tampered), len(tampered),
                                   data['public'], data['private']) != 0
print('ok')
