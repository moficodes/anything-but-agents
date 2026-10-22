# Anything But Agents — Substrate Dungeon 2D

Companion demos for "Running Anything But Agents on Agent Substrate," featuring the 8-Floor 2D Action-Roguelike (`cmd/dungeon2d`) where real-time combat and pathfinding run in the client engine while full-world state snapshots are persisted in Agent Substrate Actor Go heap RAM and GCS.

## Language

### Substrate & Persistence

**Actor**:
A gVisor-sandboxed Go process managed by Agent Substrate whose entire heap memory (`ActorSession`) is checkpointed to and restored from GCS.
_Avoid_: Container, pod, server instance

**World State**:
The complete, self-contained serializable snapshot of a player's dungeon run—including current floor, hero position, HP, class, cooldowns, inventory, equipped relics, and the position, HP, aggro state, and cleared/unlocked status of every enemy, item, lever, gate, and trap across all floors.
_Avoid_: Game state, partial delta, milestone event

**Snapshot Version**:
An immutable, numbered checkpoint (`v1..vN`) of the `World State` stored in the `Actor` Go heap and committed to GCS when the player triggers a Save or activates a Save Shrine.
_Avoid_: Save file, database row, backup

**Save-Scum Revert**:
Restoring an `Actor` (and re-hydrating the client engine's `World State`) to the latest or a chosen historical `Snapshot Version` (`v1..vN`) via `kubectl ate revert`.
_Avoid_: Rollback, undo, respawn

**Timeline Fork**:
Cloning a running or suspended `Actor` from a tagged `Snapshot Version` (`kubectl ate create tag` + `--tag`) into a new independent `Actor` ID so the player can explore mutually exclusive dungeon branches.
_Avoid_: Branching, duplicate account, copy

### Dungeon & Gameplay

**Floor**:
One of the 8 progressive levels (`Floor 1` through `Floor 8`) of the dungeon campaign, each with a multi-path tilemap, distinct tactical mechanics, and stairs connecting adjacent floors.
_Avoid_: Stage, map, room, world

**Hero Class**:
One of the three asymmetric playable archetypes—**Paladin** (high-HP armored melee tank), **Ranger** (mobile long-range bow skirmisher with slow draw), or **Wizard** (fragile glass-cannon spellcaster with armor-piercing AoE fireballs).
_Avoid_: Role, character type, job

**Primary Attack**:
A class-specific weapon or spell strike governed by attack range, line-of-sight, and an attack-speed cooldown timer.
_Avoid_: Bump attack, basic hit, auto-clash

**Class Skill**:
A secondary tactical ability on a cooldown (**Aegis Shield** for Paladin, **Tumble & Snare** for Ranger, **Frost Blink** for Wizard) used for mitigation, kiting, or repositioning.
_Avoid_: Ultimate, special power, spell slot

**Enemy Archetype**:
A monster category with distinct movement speed, aggro/line-of-sight detection, A* pathfinding behavior, armor resistance, and timed melee or ranged attack patterns (e.g., **Restless Skeleton**, **Bone Archer**, **Crypt Ghoul**, **Shadow Knight**, **Dark Acolyte**, **Abyssal Warden**, **Ancient Heap Dragon**).
_Avoid_: Mob, creep, NPC

**Save Shrine**:
An interactive sanctuary tile on a `Floor` that restores the hero's HP and triggers an immediate `World State` checkpoint to the `Actor` and GCS.
_Avoid_: Bonfire, checkpoint flag, heal station
