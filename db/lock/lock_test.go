package lock_test

import (
	"errors"
	"sync"
	"time"

	"code.cloudfoundry.org/lager/lagertest"
	"github.com/concourse/atc"
	"github.com/concourse/atc/db"
	"github.com/concourse/atc/db/lock"
	"github.com/concourse/atc/db/lock/lockfakes"
	"github.com/jackc/pgx"
	"github.com/lib/pq"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Locks", func() {
	var (
		dbConn            db.Conn
		listener          *pq.Listener
		fakeConnector     *lockfakes.FakeConnector
		pgxConn           *pgx.Conn
		pipelineDBFactory db.PipelineDBFactory
		teamDBFactory     db.TeamDBFactory
		lockFactory       lock.LockFactory
		sqlDB             *db.SQLDB

		dbLock     lock.Lock
		pipelineDB db.PipelineDB
		teamDB     db.TeamDB

		logger *lagertest.TestLogger
	)

	BeforeEach(func() {
		postgresRunner.Truncate()

		dbConn = db.Wrap(postgresRunner.Open())

		listener = pq.NewListener(postgresRunner.DataSourceName(), time.Second, time.Minute, nil)
		Eventually(listener.Ping, 5*time.Second).ShouldNot(HaveOccurred())
		bus := db.NewNotificationsBus(listener, dbConn)

		logger = lagertest.NewTestLogger("test")

		pgxConn = postgresRunner.OpenPgx()
		fakeConnector = new(lockfakes.FakeConnector)
		retryableConn := &lock.RetryableConn{Connector: fakeConnector, Conn: pgxConn}

		lockFactory = lock.NewLockFactory(retryableConn)
		sqlDB = db.NewSQL(dbConn, bus, lockFactory)
		pipelineDBFactory = db.NewPipelineDBFactory(dbConn, bus, lockFactory)

		teamDBFactory = db.NewTeamDBFactory(dbConn, bus, lockFactory)
		teamDB = teamDBFactory.GetTeamDB(atc.DefaultTeamName)

		_, err := sqlDB.CreateTeam(db.Team{Name: "some-team"})
		Expect(err).NotTo(HaveOccurred())
		teamDB := teamDBFactory.GetTeamDB("some-team")

		pipelineConfig := atc.Config{
			Resources: atc.ResourceConfigs{
				{
					Name: "some-resource",
					Type: "some-type",
					Source: atc.Source{
						"source-config": "some-value",
					},
				},
			},
			ResourceTypes: atc.ResourceTypes{
				{
					Name: "some-resource-type",
					Type: "some-type",
					Source: atc.Source{
						"source-config": "some-value",
					},
				},
			},
			Jobs: atc.JobConfigs{
				{
					Name: "some-job",
				},
			},
		}

		savedPipeline, _, err := teamDB.SaveConfigToBeDeprecated("pipeline-name", pipelineConfig, 0, db.PipelineUnpaused)
		Expect(err).NotTo(HaveOccurred())

		pipelineDB = pipelineDBFactory.Build(savedPipeline)
		dbLock = lockFactory.NewLock(logger, lock.LockID{42})
	})

	AfterEach(func() {
		err := dbConn.Close()
		Expect(err).NotTo(HaveOccurred())

		err = listener.Close()
		Expect(err).NotTo(HaveOccurred())

		dbLock.Release()
	})

	Describe("locks in general", func() {
		It("Acquire can only obtain lock once", func() {
			acquired, err := dbLock.Acquire()
			Expect(err).NotTo(HaveOccurred())
			Expect(acquired).To(BeTrue())

			acquired, err = dbLock.Acquire()
			Expect(err).NotTo(HaveOccurred())
			Expect(acquired).To(BeFalse())
		})

		It("Acquire accepts list of ids", func() {
			dbLock = lockFactory.NewLock(logger, lock.LockID{42, 56})

			Consistently(func() error {
				connCount := 3

				var anyError error
				var wg sync.WaitGroup
				wg.Add(connCount)

				for i := 0; i < connCount; i++ {
					go func() {
						defer wg.Done()

						_, err := dbLock.Acquire()
						if err != nil {
							anyError = err
						}

					}()
				}

				wg.Wait()

				return anyError
			}, 1500*time.Millisecond, 100*time.Millisecond).ShouldNot(HaveOccurred())

			dbLock = lockFactory.NewLock(logger, lock.LockID{56, 42})

			acquired, err := dbLock.Acquire()
			Expect(err).NotTo(HaveOccurred())
			Expect(acquired).To(BeTrue())

			acquired, err = dbLock.Acquire()
			Expect(err).NotTo(HaveOccurred())
			Expect(acquired).To(BeFalse())
		})

		Context("when another connection is holding the lock", func() {
			var lockFactory2 lock.LockFactory

			BeforeEach(func() {
				pgxConn2 := postgresRunner.OpenPgx()
				retryableConn2 := &lock.RetryableConn{Connector: fakeConnector, Conn: pgxConn2}
				lockFactory2 = lock.NewLockFactory(retryableConn2)
			})

			It("does not acquire the lock", func() {
				acquired, err := dbLock.Acquire()
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				dbLock2 := lockFactory2.NewLock(logger, lock.LockID{42})
				acquired, err = dbLock2.Acquire()
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeFalse())

				dbLock.Release()
				dbLock2.Release()
			})

			It("acquires the locks once it is released", func() {
				acquired, err := dbLock.Acquire()
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				dbLock2 := lockFactory2.NewLock(logger, lock.LockID{42})
				acquired, err = dbLock2.Acquire()
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeFalse())

				dbLock.Release()

				acquired, err = dbLock2.Acquire()
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				dbLock2.Release()
			})
		})

		Context("when two locks are being acquired at the same time", func() {
			var lock1 lock.Lock
			var lock2 lock.Lock
			var fakeLockDB *lockfakes.FakeLockDB
			var acquiredLock2 chan struct{}
			var lock2Err error
			var lock2Acquired bool

			BeforeEach(func() {
				fakeLockDB = new(lockfakes.FakeLockDB)
				fakeLockFactory := lock.NewTestLockFactory(fakeLockDB)
				lock1 = fakeLockFactory.NewLock(logger, lock.LockID{57})
				lock2 = fakeLockFactory.NewLock(logger, lock.LockID{57})

				acquiredLock2 = make(chan struct{})
			})

			JustBeforeEach(func() {
				called := false
				readyToAcquire := make(chan struct{})

				fakeLockDB.AcquireStub = func(id lock.LockID) (bool, error) {
					if !called {
						called = true

						go func() {
							close(readyToAcquire)
							lock2Acquired, lock2Err = lock2.Acquire()
							close(acquiredLock2)
						}()

						<-readyToAcquire
					}

					return true, nil
				}
			})

			It("only acquires one of the locks", func() {
				acquired, err := lock1.Acquire()
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				<-acquiredLock2

				Expect(lock2Err).NotTo(HaveOccurred())
				Expect(lock2Acquired).To(BeFalse())
			})

			Context("when locks are being created on different lock factory (different db conn)", func() {
				BeforeEach(func() {
					fakeLockFactory2 := lock.NewTestLockFactory(fakeLockDB)
					lock2 = fakeLockFactory2.NewLock(logger, lock.LockID{57})
				})

				It("allows to acquire both locks", func() {
					acquired, err := lock1.Acquire()
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					<-acquiredLock2

					Expect(lock2Err).NotTo(HaveOccurred())
					Expect(lock2Acquired).To(BeTrue())
				})
			})
		})

		Context("connection died", func() {
			var pgxConn1 *pgx.Conn

			Context("when it can create connection to db", func() {
				BeforeEach(func() {
					pgxConn1 = postgresRunner.OpenPgx()
					fakeConnector.ConnectReturns(pgxConn1, nil)
				})

				It("recreates connection on Acquire", func() {
					err := pgxConn.Close()
					Expect(err).NotTo(HaveOccurred())

					acquired, err := dbLock.Acquire()
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					acquired, err = dbLock.Acquire()
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeFalse())

					Expect(fakeConnector.ConnectCallCount()).To(Equal(1))
				})

				It("recreates connection but returns ErrLostLock on release", func() {
					acquired, err := dbLock.Acquire()
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					err = pgxConn.Close()
					Expect(err).NotTo(HaveOccurred())

					Eventually(func() int {
						var count int
						err := pgxConn1.QueryRow(`SELECT count(*) FROM pg_locks WHERE objid = 42`).Scan(&count)
						Expect(err).ToNot(HaveOccurred())
						return count
					}).Should(BeZero())

					err = dbLock.Release()
					Expect(err).To(Equal(lock.ErrLostLock))

					acquired, err = dbLock.Acquire()
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					Expect(fakeConnector.ConnectCallCount()).To(Equal(1))
				})
			})

			Context("when it cannot create connection to db", func() {
				BeforeEach(func() {
					count := 0
					fakeConnector.ConnectStub = func() (lock.DelegateConn, error) {
						if count == 0 {
							count++
							return nil, errors.New("disaster")
						} else {
							return postgresRunner.OpenPgx(), nil
						}
					}
				})

				It("keeps trying to reconnect", func() {
					err := pgxConn.Close()
					Expect(err).NotTo(HaveOccurred())

					acquired, err := dbLock.Acquire()
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					err = dbLock.Release()
					Expect(err).NotTo(HaveOccurred())

					acquired, err = dbLock.Acquire()
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					Expect(fakeConnector.ConnectCallCount()).To(Equal(2))
				})
			})
		})
	})

	Describe("taking out a lock on pipeline scheduling", func() {
		Context("when it has been scheduled recently", func() {
			It("does not get the lock", func() {
				lock, acquired, err := pipelineDB.AcquireSchedulingLock(logger, 1*time.Second)
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				lock.Release()

				_, acquired, err = pipelineDB.AcquireSchedulingLock(logger, 1*time.Second)
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeFalse())
			})
		})

		Context("when there has not been any scheduling recently", func() {
			It("gets and keeps the lock and stops others from getting it", func() {
				lock, acquired, err := pipelineDB.AcquireSchedulingLock(logger, 1*time.Second)
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				Consistently(func() bool {
					_, acquired, err = pipelineDB.AcquireSchedulingLock(logger, 1*time.Second)
					Expect(err).NotTo(HaveOccurred())

					return acquired
				}, 1500*time.Millisecond, 100*time.Millisecond).Should(BeFalse())

				lock.Release()

				time.Sleep(time.Second)

				newLock, acquired, err := pipelineDB.AcquireSchedulingLock(logger, 1*time.Second)
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				newLock.Release()
			})
		})
	})

	Describe("GetPendingBuildsForJob/GetAllPendingBuilds", func() {
		Context("when a build is created", func() {
			BeforeEach(func() {
				_, err := pipelineDB.CreateJobBuild("some-job")
				Expect(err).NotTo(HaveOccurred())
			})

			It("returns the build", func() {
				pendingBuildsForJob, err := pipelineDB.GetPendingBuildsForJob("some-job")
				Expect(err).NotTo(HaveOccurred())
				Expect(pendingBuildsForJob).To(HaveLen(1))

				pendingBuilds, err := pipelineDB.GetAllPendingBuilds()
				Expect(err).NotTo(HaveOccurred())
				Expect(pendingBuilds).To(HaveLen(1))
				Expect(pendingBuilds["some-job"]).NotTo(BeNil())
			})
		})
	})

	Describe("EnsurePendingBuildExists", func() {
		Context("when only a started build exists", func() {
			BeforeEach(func() {
				build1, err := pipelineDB.CreateJobBuild("some-job")
				Expect(err).NotTo(HaveOccurred())

				started, err := build1.Start("some-engine", "some-metadata")
				Expect(err).NotTo(HaveOccurred())
				Expect(started).To(BeTrue())
			})

			It("creates a build", func() {
				err := pipelineDB.EnsurePendingBuildExists("some-job")
				Expect(err).NotTo(HaveOccurred())

				pendingBuildsForJob, err := pipelineDB.GetPendingBuildsForJob("some-job")
				Expect(err).NotTo(HaveOccurred())
				Expect(pendingBuildsForJob).To(HaveLen(1))
			})

			It("doesn't create another build the second time it's called", func() {
				err := pipelineDB.EnsurePendingBuildExists("some-job")
				Expect(err).NotTo(HaveOccurred())

				err = pipelineDB.EnsurePendingBuildExists("some-job")
				Expect(err).NotTo(HaveOccurred())

				builds2, err := pipelineDB.GetPendingBuildsForJob("some-job")
				Expect(err).NotTo(HaveOccurred())
				Expect(builds2).To(HaveLen(1))

				started, err := builds2[0].Start("some-engine", "some-metadata")
				Expect(err).NotTo(HaveOccurred())
				Expect(started).To(BeTrue())

				builds2, err = pipelineDB.GetPendingBuildsForJob("some-job")
				Expect(err).NotTo(HaveOccurred())
				Expect(builds2).To(HaveLen(0))
			})
		})
	})

	Describe("AcquireResourceTypeCheckingLock", func() {
		var someResourceType db.SavedResourceType

		BeforeEach(func() {
			var err error
			var found bool
			someResourceType, found, err = pipelineDB.GetResourceType("some-resource-type")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
		})

		Context("when there has been a check recently", func() {
			Context("when acquiring immediately", func() {
				It("gets the lock", func() {
					var acquired bool
					var err error
					dbLock, acquired, err = pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, false)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					dbLock.Release()

					dbLock, acquired, err = pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, true)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())
				})
			})

			Context("when not acquiring immediately", func() {
				It("does not get the lock", func() {
					var acquired bool
					var err error
					dbLock, acquired, err = pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, false)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					dbLock.Release()

					_, acquired, err = pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, false)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeFalse())
				})
			})
		})

		Context("when there has not been a check recently", func() {
			Context("when acquiring immediately", func() {
				It("gets and keeps the lock and stops others from periodically getting it", func() {
					lock, acquired, err := pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, true)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					Consistently(func() bool {
						_, acquired, err = pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, false)
						Expect(err).NotTo(HaveOccurred())

						return acquired
					}, 1500*time.Millisecond, 100*time.Millisecond).Should(BeFalse())

					lock.Release()

					time.Sleep(time.Second)

					newLock, acquired, err := pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, true)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					newLock.Release()
				})

				It("gets and keeps the lock and stops others from immediately getting it", func() {
					lock, acquired, err := pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, true)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					Consistently(func() bool {
						_, acquired, err = pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, true)
						Expect(err).NotTo(HaveOccurred())

						return acquired
					}, 1500*time.Millisecond, 100*time.Millisecond).Should(BeFalse())

					lock.Release()

					time.Sleep(time.Second)

					newLock, acquired, err := pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, true)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					newLock.Release()
				})
			})

			Context("when not acquiring immediately", func() {
				It("gets and keeps the lock and stops others from periodically getting it", func() {
					lock, acquired, err := pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, false)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					Consistently(func() bool {
						_, acquired, err = pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, false)
						Expect(err).NotTo(HaveOccurred())

						return acquired
					}, 1500*time.Millisecond, 100*time.Millisecond).Should(BeFalse())

					lock.Release()

					time.Sleep(time.Second)

					newLock, acquired, err := pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, false)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					newLock.Release()
				})

				It("gets and keeps the lock and stops others from immediately getting it", func() {
					lock, acquired, err := pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, false)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					Consistently(func() bool {
						_, acquired, err = pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, true)
						Expect(err).NotTo(HaveOccurred())

						return acquired
					}, 1500*time.Millisecond, 100*time.Millisecond).Should(BeFalse())

					lock.Release()

					time.Sleep(time.Second)

					newLock, acquired, err := pipelineDB.AcquireResourceTypeCheckingLock(logger, someResourceType, 1*time.Second, false)
					Expect(err).NotTo(HaveOccurred())
					Expect(acquired).To(BeTrue())

					newLock.Release()
				})
			})
		})
	})

	Describe("taking out a lock on build tracking", func() {
		var build db.Build

		BeforeEach(func() {
			var err error
			build, err = teamDB.CreateOneOffBuild()
			Expect(err).NotTo(HaveOccurred())
		})

		It("gets and keeps the lock and stops others from getting it", func() {
			lock, acquired, err := build.AcquireTrackingLock(logger, 1*time.Second)
			Expect(err).NotTo(HaveOccurred())
			Expect(acquired).To(BeTrue())

			Consistently(func() bool {
				_, acquired, err = build.AcquireTrackingLock(logger, 1*time.Second)
				Expect(err).NotTo(HaveOccurred())

				return acquired
			}, 1500*time.Millisecond, 100*time.Millisecond).Should(BeFalse())

			lock.Release()

			time.Sleep(time.Second)

			newLock, acquired, err := build.AcquireTrackingLock(logger, 1*time.Second)
			Expect(err).NotTo(HaveOccurred())
			Expect(acquired).To(BeTrue())

			newLock.Release()
		})
	})

	Describe("GetTaskLock", func() {
		Context("when something got the lock recently", func() {
			It("does not get the lock", func() {
				lock, acquired, err := sqlDB.GetTaskLock(logger, "some-task-name")
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				_, acquired, err = sqlDB.GetTaskLock(logger, "some-task-name")
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeFalse())

				lock.Release()
			})
		})

		Context("when no one got the lock recently", func() {
			It("gets and keeps the lock and stops others from getting it", func() {
				lock, acquired, err := sqlDB.GetTaskLock(logger, "some-task-name")
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				Consistently(func() bool {
					_, acquired, err = sqlDB.GetTaskLock(logger, "some-task-name")
					Expect(err).NotTo(HaveOccurred())

					return acquired
				}, 1500*time.Millisecond, 100*time.Millisecond).Should(BeFalse())

				lock.Release()
			})
		})

		Context("when something got a different lock recently", func() {
			It("still gets the lock", func() {
				lock, acquired, err := sqlDB.GetTaskLock(logger, "some-other-task-name")
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				lock.Release()

				newLock, acquired, err := sqlDB.GetTaskLock(logger, "some-task-name")
				Expect(err).NotTo(HaveOccurred())
				Expect(acquired).To(BeTrue())

				newLock.Release()
			})
		})
	})
})
