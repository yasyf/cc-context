import { useMemo, useState } from "react";

type Profile = {
  name: string;
  role: string;
  joinedAt: Date;
  skills: string[];
};

type ProfileCardProps = {
  profile: Profile;
  onMessage: (name: string) => void;
};

export function ProfileCard({ profile, onMessage }: ProfileCardProps) {
  const [expanded, setExpanded] = useState(false);
  const membership = useMemo(
    () => `Member since ${profile.joinedAt.getFullYear()}`,
    [profile.joinedAt],
  );

  return (
    <article className="profile-card" aria-label={`${profile.name}'s profile`}>
      <header>
        <div>
          <h2>{profile.name}</h2>
          <p>{profile.role}</p>
        </div>
        <button type="button" onClick={() => onMessage(profile.name)}>
          Send message
        </button>
      </header>

      <p className="profile-card__membership">{membership}</p>
      <button type="button" onClick={() => setExpanded((value) => !value)}>
        {expanded ? "Hide skills" : "Show skills"}
      </button>

      {expanded && (
        <ul>
          {profile.skills.map((skill) => (
            <li key={skill}>{skill}</li>
          ))}
        </ul>
      )}
    </article>
  );
}
