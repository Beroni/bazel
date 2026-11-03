/**
 * BUDGET is how long a test waits on a chain of processes it does not drive: a
 * clone and a checkout through the fake gh, an agent spawn, a whole job, a server
 * boot, a browser's EventSource retry. The suite runs 6 workers on a machine it
 * shares with other agents, and there each link takes its time: with the load
 * average near 100 on 8 cores, a checkout took 41s to become an agent process,
 * and a 3s EventSource retry fired after 11s (test/e2e-stability).
 *
 * A wait that depends on such a chain uses this, and waits on the event itself
 * (the agent's record, the job's state), never on a guess of how long the step
 * before it takes. A passing test never waits for the budget.
 */
export const BUDGET = 60_000;
