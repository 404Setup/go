# The Go Programming Language Illium

Go is an open source programming language that makes it easy to build simple,
reliable, and efficient software.

This fork is a personal Go branch maintained by 404Setup.

## Feature

> This fork will not release any updates that modify the syntax or break existing APIs.

- std: sync/v2 - It implements generic versions of `sync.Pool` and `sync.Map`, as well as an atomic ordered map.
- std: sync/atomic/v2 - Implemented a generic version of `atomic.Value`
- std: container/v2 - Implemented ordered containers
- gc: Slightly optimized some parts of sync.Pool (I'm not sure if it works because its commits are mixed up with other commits, so I can't test it)
- compiler: To resolve a passive, malicious memory leak bug from Kaspersky, GOFIPS140 was actually removed from the binary when it was disabled. Previously, it would always be compiled instead of being eliminated by the dead code eliminater.

None of the commits will be merged upstream because I cannot guarantee the quality of my code and I do not have time 
to wait for the pr queue.

Want to set it up in actions? We'll be releasing an action soon so you can seamlessly migrate to our fork. Until then, 
you can check out the examples in my project, which are licensed under the MPL-2.0 license. Once released as a release, 
it will be licensed under the Apache-2.0 license.

Before reporting an issue, you should confirm whether the problem originated with us or our upstream. You should not report 
bugs caused by us to our upstream.

We use streaming version updates, meaning we merge whatever our upstream updates in the master branch. 
We do not maintain LTS versions.

## Download and Install

### Binary Distributions

Official binary distributions are available at https://github.com/404Setup/go/releases.

### Install From Source

If a binary distribution is not available for your combination of
operating system and architecture, visit
https://go.dev/doc/install/source
for source installation instructions.