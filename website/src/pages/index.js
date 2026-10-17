import Link from '@docusaurus/Link';
import Layout from '@theme/Layout';

const steps = [
  ['Detect', 'An engine member or its Pod fails.'],
  ['Shrink', 'The engine drops the dead member and the rest keep serving.'],
  ['Reset', 'The failed Pod is reset in place.'],
  ['Admit', 'The new member joins and the group is back to full capacity.'],
];

export default function Home() {
  return (
    <Layout description="Recover LLM inference engines from hardware faults on Kubernetes.">
      <main className="container home">
        <h1>
          Your GPUs will fail. <span>Plan accordingly.</span>
        </h1>
        <p>
          The llm-d Inference Resiliency Operator (IRO) provides automated failure detection and recovery for distributed
          LLM inference on Kubernetes. It shrinks the engine around the failure, keeps it serving, and
          brings it back to full capacity without manual intervention.
        </p>
        <div className="home__buttons">
          <Link className="button button--primary" to="/docs">Read the docs</Link>
          <Link className="button button--outline button--primary" href="https://github.com/llm-d-incubation/llm-d-resiliency-operator">GitHub</Link>
        </div>
        <div className="home__steps">
          {steps.map(([title, text]) => (
            <div key={title}>
              <b>{title}</b>
              <p>{text}</p>
            </div>
          ))}
        </div>
      </main>
    </Layout>
  );
}
