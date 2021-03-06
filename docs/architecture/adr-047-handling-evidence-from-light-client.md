# ADR 047: Handling evidence from light client

## Changelog
* 18-02-2020: Initial draft
* 24-02-2020: Second version
* 13-04-2020: Add PotentialAmnesiaEvidence and a few remarks
* 31-07-2020: Remove PhantomValidatorEvidence

## Context

If the light client is under attack, either directly -> lunatic
attack (light fork) or indirectly -> full fork, it's supposed to halt and
send evidence of misbehavior to a correct full node. Upon receiving an
evidence, the full node should punish malicious validators (if possible).

## Decision

When a light client sees two conflicting headers (`H1.Hash() != H2.Hash()`,
`H1.Height == H2.Height`), both having 1/3+ of the voting power of the
currently trusted validator set, it will submit a `ConflictingHeadersEvidence`
to all full nodes it's connected to. Evidence needs to be submitted to all full
nodes since there's no way to determine which full node is correct (honest).

```go
type ConflictingHeadersEvidence struct {
  H1 types.SignedHeader
  H2 types.SignedHeader
}
```

_Remark_: Theoretically, only the header, which differs from what a full node
has, needs to be sent. But sending two headers a) makes evidence easily
verifiable b) simplifies the light client, which does not have query each
witness as to which header it possesses.

When a full node receives the `ConflictingHeadersEvidence` evidence, it should
a) validate it b) figure out if malicious behaviour is obvious (immediately
slashable) or the fork accountability protocol needs to be started.

### Validating headers

Check both headers are valid (`ValidateBasic`), have the same height, and
signed by 1/3+ of the validator set that the full node had at height
`H1.Height`.

- Q: What if light client validator set is not equal to full node's validator
  set (i.e. from full node's point of view both headers are not properly signed;
  this includes the case where none of the two headers were committed on the
  main chain)

  Reject the evidence. It means light client is following a fork, but, hey, at
  least it will halt.

- Q: Don't we want to punish validators who signed something else even if they
  have less or equal than 1/3?

  No consensus so far. Ethan said no, Zarko said yes.
  https://github.com/tendermint/spec/pull/71#discussion_r374210533

### Figuring out if malicious behaviour is immediately slashable

Let's say H1 was committed from this full node's perspective (see Appendix A).
_If neither of the headers (H1 and H2) were committed from the full node's
perspective, the evidence must be rejected._

Intersect validator sets of H1 and H2.

* if there are signers(H2) that are not part of validators(H1), they misbehaved as
they are signing protocol messages in heights they are not validators =>
immediately slashable (#F4).

* if `H1.Round == H2.Round`, and some signers signed different precommit
messages in both commits, then it is an equivocation misbehavior => immediately
slashable (#F1).

* if `H1.Round != H2.Round` we need to run full detection procedure => not
immediately slashable.

* if `ValidatorsHash`, `NextValidatorsHash`, `ConsensusHash`,
`AppHash`, and `LastResultsHash` in H2 are different (incorrect application
state transition), then it is a lunatic misbehavior => immediately slashable (#F5).

If evidence is not immediately slashable, fork accountability needs to invoked
(ADR does not yet exist).

It's unclear if we should further break up `ConflictingHeadersEvidence` or
gossip and commit it directly. See
https://github.com/tendermint/tendermint/issues/4182#issuecomment-590339233

If we'd go without breaking evidence, all we'll need to do is to strip the
committed header from `ConflictingHeadersEvidence` (H1) and leave only the
uncommitted header (H2):

```go
type ConflictingHeaderEvidence struct {
  H types.SignedHeader
}
```

If we'd go with breaking evidence, here are the types we'll need:

### F1. Equivocation

Existing `DuplicateVoteEvidence` needs to be created and gossiped.


### F5. Lunatic validator

```go
type LunaticValidatorEvidence struct {
	Header             types.Header
	Vote               types.Vote
	InvalidHeaderField string
}
```

To punish this attack, we need support for a new Evidence type -
`LunaticValidatorEvidence`. This type includes a vote and a header. The header
must contain fields that are invalid with respect to the previous block, and a
vote for that header by a validator that was in a validator set within the
unbonding period. While the attack is only possible if +1/3 of some validator
set colludes, the evidence should be verifiable independently for each
individual validator. This means the total evidence can be split into one piece
of evidence per attacking validator and gossipped to nodes to be verified one
piece at a time, reducing the DoS attack surface at the peer layer.

Note it is not sufficient to simply compare this header with that committed for
the corresponding height, as an honest node may vote for a header that is not
ultimately committed. Certain fields may also be variable, for instance the
`LastCommitHash` and the `Time` may depend on which votes the proposer includes.
Thus, the header must be explicitly checked for invalid data.

For the attack to succeed, VC must sign a header that changes the validator set
to consist of something they control. Without doing this, they can not
otherwise attack the light client, since the client verifies commits according
to validator sets. Thus, it should be sufficient to check only that
`ValidatorsHash` and `NextValidatorsHash` are correct with respect to the
header that was committed at the corresponding height.

That said, if the attack is conducted by +2/3 of the validator set, they don't
need to make an invalid change to the validator set, since they already control
it. Instead they would make invalid changes to the `AppHash`, or possibly other
fields. In order to punish them, then, we would have to check all header
fields.

Note some header fields require the block itself to verify, which the light
client, by definition, does not possess, so it may not be possible to check
these fields. For now, then, `LunaticValidatorEvidence` must be checked against
all header fields which are a function of the application at previous blocks.
This includes `ValidatorsHash`, `NextValidatorsHash`, `ConsensusHash`,
`AppHash`, and `LastResultsHash`. These should all match what's in the header
for the block that was actually committed at the corresponding height, and
should thus be easy to check.

`InvalidHeaderField` contains the invalid field name. Note it's very likely
that multiple fields diverge, but it's faster to check just one. This field
MUST NOT be used to determine equality of `LunaticValidatorEvidence`.

### F2. Amnesia

```go
type PotentialAmnesiaEvidence struct {
	VoteA types.Vote
	VoteB types.Vote
}
```

To punish this attack, votes under question needs to be sent. Fork
accountability process should then use this evidence to request additional
information from offended validators and construct a new type of evidence to
punish those who conducted an amnesia attack.

See ADR-056 for the architecture of the handling amnesia attacks.

NOTE: Conflicting headers evidence used to also create PhantomValidatorEvidence
but this has since been removed. Refer to Appendix B.  

## Status

Proposed.

## Consequences

### Positive

* Tendermint will be able to detect & punish new types of misbehavior
* light clients connected to multiple full nodes can help full nodes notice a
  fork faster

### Negative

* Accepting `ConflictingHeadersEvidence` from light clients opens up a DDOS
attack vector (same is fair for any RPC endpoint open to public; remember that
RPC is not open by default).

### Neutral

## References

* [Fork accountability spec](https://github.com/tendermint/spec/blob/master/spec/consensus/light-client/accountability.md)

## Appendix A

If there is an actual fork (full fork), a full node may follow either one or
another branch. So both H1 or H2 can be considered committed depending on which
branch the full node is following. It's supposed to halt if it notices an
actual fork, but there's a small chance it doesn't.

## Appendix B

PhantomValidatorEvidence was used to capture when a validator that was still staked
(i.e. within the bonded period) but was not in the current validator set had voted for a block.

In later discussions it was argued that although possible to keep phantom validator
evidence, any case a phantom validator that could have the capacity to be involved
in fooling a light client would have to be aided by 1/3+ lunatic validators.

It would also be very unlikely that the new validators injected by the lunatic attack
would be validators that currently still have something staked.

Not only this but there was a large degree of extra computation required in storing all
the currently staked validators that could possibly fall into the group of being
a phantom validator. Given this, it was removed.
