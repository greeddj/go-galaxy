// Package lockaudit is a test-only gate: every function that takes the cache
// backend's exclusive lock must run its work under the holder context the lock
// returned and judge the outcome through cacheManager.LockLostError.
package lockaudit
